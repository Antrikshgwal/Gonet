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
	// pprof on a separate localhost port for profiling under load.
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
		addr = ":8080"
	}
	_, port, _ := net.SplitHostPort(addr)
	if port == "" {
		port = "8080"
	}

	// Keep the practice arena alive with a shifting population of bots.
	go populatePractice("ws://127.0.0.1:"+port+"/ws?mode=practice", bot.LoadMLP(gonet.BotModel))

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", serveWS(pvp, practice))

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

// populatePractice maintains a shifting population of bots in the practice arena
// so it feels like a live lobby: they spawn on random timers, each with a random
// skill and a random lifetime, and a mix of the MLP and the heuristic.
func populatePractice(wsURL string, model *bot.MLP) {
	var live atomic.Int64
	const target = 5
	spawn := func() {
		if live.Add(1) > target {
			live.Add(-1)
			return
		}
		m := model
		if rand.Float64() < 0.5 {
			m = nil // mix in heuristic-only bots for variety
		}
		aggro := 0.3 + rand.Float64()*0.6
		life := time.Duration(20+rand.Intn(70)) * time.Second
		go func() {
			defer live.Add(-1)
			ctx, cancel := context.WithTimeout(context.Background(), life)
			defer cancel()
			bot.Play(ctx, wsURL, m, aggro)
		}()
	}
	time.Sleep(time.Second) // let the HTTP server come up before bots dial it
	for range 3 {           // seed so a visitor lands in a populated arena
		spawn()
	}
	for {
		time.Sleep(time.Duration(2000+rand.Intn(4000)) * time.Millisecond)
		spawn()
	}
}

// serveWS routes a connection to the PvP arena, or the practice arena when the
// client asks for ?mode=practice.
func serveWS(pvp, practice *hub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := pvp
		if r.URL.Query().Get("mode") == "practice" {
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
