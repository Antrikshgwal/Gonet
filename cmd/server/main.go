package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
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

	h := hub.New()

	// MAX_PLAYERS caps concurrent connections so the O(n²) single arena can't be
	// driven into the ground (default 200).
	if n, err := strconv.Atoi(os.Getenv("MAX_PLAYERS")); err == nil {
		h.SetMaxPlayers(n)
	}

	if path := os.Getenv("RECORD"); path != "" {
		// Append so recordings accumulate across sessions instead of being
		// truncated each run.
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
		h.Record(func(s hub.Sample) {
			select {
			case ch <- s:
			default:
			}
		})
		log.Printf("recording samples to %s", path)
	}

	go h.Run()

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", serveWS(h))

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Lobby: a JSON view of who's connected. Control-plane endpoint for tools
	// and humans, so it's plain HTTP/JSON rather than the binary WS protocol.
	mux.HandleFunc("/lobby", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Lobby())
	})

	// /bot spawns an in-process bot that dials our own /ws — the same way a
	// browser connects — so the deployed "play vs bot" button needs no shell.
	// Defaults to the embedded MLP, capped so it can't be spammed.
	botModel := bot.LoadMLP(gonet.BotModel)
	_, port, _ := net.SplitHostPort(addr)
	if port == "" {
		port = "8080"
	}
	wsURL := "ws://127.0.0.1:" + port + "/ws"
	var activeBots atomic.Int64
	const maxBots = 4
	mux.HandleFunc("/bot", func(w http.ResponseWriter, r *http.Request) {
		if activeBots.Add(1) > maxBots {
			activeBots.Add(-1)
			http.Error(w, "too many bots", http.StatusTooManyRequests)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			defer activeBots.Add(-1)
			if err := bot.Play(ctx, wsURL, botModel); err != nil {
				log.Printf("bot: %v", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("bot joining"))
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

func serveWS(h *hub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
				log.Printf("Read error: %v", err)
				break
			}

			var in hub.InputMsg
			if err := msgpack.Unmarshal(message, &in); err != nil {
				log.Printf("Bad input message: %v", err)
				continue
			}

			h.SendInput(conn, in)
		}
	}
}
