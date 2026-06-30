package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on the default mux
	"os"
	"strconv"
	"sync/atomic"
	"time"

	gonet "github.com/Antrikshgwal/gonet"
	"github.com/Antrikshgwal/gonet/internal/bot"
	"github.com/Antrikshgwal/gonet/internal/hub"
	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func main() {
	go func() { log.Println(http.ListenAndServe("localhost:6060", nil)) }()

	// Two isolated arenas: real players never share a hub with bots, so nobody
	// can inject bots into someone's PvP match. Practice is populated by the
	// server (see populatePractice); the client opts in with ?mode=practice.
	pvp := hub.New()
	practice := hub.New()

	// MAX_PLAYERS caps each arena so the O(n²) hub can't be driven into the ground.
	if n, err := strconv.Atoi(os.Getenv("MAX_PLAYERS")); err == nil {
		pvp.SetMaxPlayers(n)
		practice.SetMaxPlayers(n)
	}

	// RECORD captures (state, action) pairs — from real PvP play, not the bots.
	if path := os.Getenv("RECORD"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("open record file: %v", err)
		}
		ch := make(chan hub.Sample, 4096)
		go func() {
			enc := json.NewEncoder(f)
			for s := range ch {
				enc.Encode(s)
			}
		}()
		pvp.Record(func(s hub.Sample) {
			select {
			case ch <- s:
			default:
			}
		})
		log.Printf("recording samples to %s", path)
	}

	go pvp.Run()
	go practice.Run()

	addr := os.Getenv("ADDR")
	if addr == "" {
		if p := os.Getenv("PORT"); p != "" {
			addr = ":" + p
		} else {
			addr = ":8080"
		}
	}
	_, port, _ := net.SplitHostPort(addr)
	if port == "" {
		port = "8080"
	}

	// Populate the practice arena with bots — but only while a human is in it,
	// so an idle deploy doesn't burn resources playing bots against nobody.
	var practiceHumans atomic.Int64
	go populatePractice("ws://127.0.0.1:"+port+"/ws?mode=practice&bot=1", bot.LoadMLP(gonet.BotModel), &practiceHumans)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", serveWS(pvp, practice, &practiceHumans))

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Lobby: a JSON view of who's in the PvP arena. Control-plane endpoint, so
	// it's plain HTTP/JSON rather than the binary WS protocol.
	mux.HandleFunc("/lobby", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pvp.Lobby())
	})

	clientFS, err := fs.Sub(gonet.ClientFS, "client")
	if err != nil {
		log.Fatalf("failed to open embedded client FS: %v", err)
	}
	mux.Handle("/play/", http.StripPrefix("/play/", http.FileServer(http.FS(clientFS))))

	siteFS, err := fs.Sub(gonet.SiteFS, "site")
	if err != nil {
		log.Fatalf("failed to open embedded site FS: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(siteFS)))

	log.Printf("server listening on %s (ws at /ws, static embedded)", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func populatePractice(wsURL string, model *bot.MLP, humans *atomic.Int64) {
	var live atomic.Int64
	const target = 5
	for {
		time.Sleep(time.Duration(900+rand.Intn(1100)) * time.Millisecond)
		if humans.Load() == 0 || live.Load() >= target {
			continue // nobody to play, or arena already full
		}
		m := model
		if rand.Float64() < 0.5 {
			m = nil // mix in heuristic-only bots for variety
		}
		aggro := 0.3 + rand.Float64()*0.6
		life := time.Duration(20+rand.Intn(70)) * time.Second
		live.Add(1)
		go func() {
			defer live.Add(-1)
			ctx, cancel := context.WithTimeout(context.Background(), life)
			defer cancel()
			bot.Play(ctx, wsURL, m, aggro)
		}()
	}
}

// serveWS routes a connection to the PvP arena, or the practice arena when the
// client asks for ?mode=practice.
func serveWS(pvp, practice *hub.Hub, practiceHumans *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		practiceMode := r.URL.Query().Get("mode") == "practice"
		h := pvp
		if practiceMode {
			h = practice
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade connection: %v", err)
			return
		}
		if !h.Register(conn) {
			conn.WriteMessage(websocket.TextMessage, []byte("server full"))
			conn.Close()
			return
		}
		defer h.Unregister(conn)

		// Count real humans in practice (not the bots, which carry ?bot=1) so the
		// population manager only spawns when someone's there to play.
		if practiceMode && r.URL.Query().Get("bot") != "1" {
			practiceHumans.Add(1)
			defer practiceHumans.Add(-1)
		}

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var in hub.InputMsg
			if err := msgpack.Unmarshal(message, &in); err != nil {
				continue
			}
			h.SendInput(conn, in)
		}
	}
}
