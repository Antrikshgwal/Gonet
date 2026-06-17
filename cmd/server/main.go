package main

import (
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Hub struct {
register   chan *websocket.Conn
unregister chan *websocket.Conn

broadcast chan []byte

clients map[*websocket.Conn]bool
}

func  newHub() *Hub {
	return &Hub{
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		broadcast:  make(chan []byte),
		clients:    make(map[*websocket.Conn]bool),
	}
}

func (h *Hub) run() {
	for{
		select {
		case registeredConn := <-h.register:
		h.clients[registeredConn] = true
		log.Printf("Client registered: %v", registeredConn.RemoteAddr())

		case unregisteredConn := <-h.unregister:
		if _, ok := h.clients[unregisteredConn]; ok {
			delete(h.clients, unregisteredConn)
			log.Printf("Client unregistered: %v", unregisteredConn.RemoteAddr())
		}

		case message := <-h.broadcast:
		for conn := range h.clients {
			if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Printf("Failed to send message to client: %v", err)
				conn.Close()
				delete(h.clients, conn)
			}
		}
	}
	}
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

	h := newHub()
	go h.run()

	log.Println("WebSocket server listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
