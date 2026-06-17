package main

import (
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade connection: %v", err)
			return
		}
		defer conn.Close()

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Failed to read message: %v", err)
				break
			}
			log.Printf("Received message: %s", message)

			if err := conn.WriteMessage(messageType, message); err != nil {
				log.Printf("Failed to write message: %v", err)
				break
			}
		}
	})

	http.ListenAndServe(":8080", mux)
}
