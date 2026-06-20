package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func main() {
	mux := http.NewServeMux()
	h := newHub()
	go h.run()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade connection: %v", err)
			return
		}
		h.register <- conn
		defer func() { h.unregister <- conn }()

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Read error: %v", err)
				break
			}

			var in InputMsg
			if err := json.Unmarshal(message, &in); err != nil {
				log.Printf("Bad input message: %v", err)
				continue
			}
			h.input <- inputEvent{conn: conn, msg: in}
		}
	})

	log.Println("WebSocket server listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
