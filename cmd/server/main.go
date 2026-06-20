package main

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"

	gonet "github.com/Antrikshgwal/gonet"
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

	
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})


	clientFS, err := fs.Sub(gonet.ClientFS, "client")
	if err != nil {
		log.Fatalf("failed to open embedded client FS: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(clientFS)))

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
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
