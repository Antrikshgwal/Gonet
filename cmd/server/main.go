package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/Antrikshgwal/gonet/internal/hub"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	h := hub.New()
	go h.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", serveWS(h))

	// Serve the static client (index.html, etc.) from the same binary.
	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		staticDir = "./client"
	}
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("server listening on %s (ws at /ws, static from %s)", addr, staticDir)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// serveWS upgrades a request to a WebSocket and bridges the connection to the
// hub: register on connect, forward each input message, unregister on exit.
func serveWS(h *hub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade connection: %v", err)
			return
		}
		h.Register(conn)
		defer h.Unregister(conn)

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Read error: %v", err)
				break
			}

			var in hub.InputMsg
			if err := json.Unmarshal(message, &in); err != nil {
				log.Printf("Bad input message: %v", err)
				continue
			}
			h.SendInput(conn, in)
		}
	}
}
