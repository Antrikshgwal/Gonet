package main

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// InputMsg is a movement input sent by a client.
type InputMsg struct {
	Dx int8 `json:"dx"`
	Dy int8 `json:"dy"`
}

// PlayerState represents the state of a player in the game.
type PlayerState struct {
	ID string  `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	Vx float64 `json:"vx"`
	Vy float64 `json:"vy"`
}

// SnapshotMsg is the full game state sent to all clients each tick.
type SnapshotMsg struct {
	Players []PlayerState `json:"players"`
}

// inputEvent ties an input message to the connection that sent it.
type inputEvent struct {
	conn *websocket.Conn
	msg  InputMsg
}

type Hub struct {
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	input      chan inputEvent
	clients    map[*websocket.Conn]*PlayerState
}

func newHub() *Hub {
	return &Hub{
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		input:      make(chan inputEvent),
		clients:    make(map[*websocket.Conn]*PlayerState),
	}
}

func (h *Hub) run() {

	// The speed constant defines how fast players move in units per second.
	const speed = 200.0
	// The game loop runs at a fixed tick rate, integrating player positions and broadcasting the game state.
	const tickRate = 50 * time.Millisecond
	ticker := time.NewTicker(tickRate)
	defer ticker.Stop()
	dt := tickRate.Seconds()

	for {
		select {
		case conn := <-h.register:
			id := conn.RemoteAddr().String()
			// Stagger spawns so players don't stack on the same pixel.
			const spawnX, spawnY, spawnStep = 300.0, 200.0, 40.0
			x := spawnX + float64(len(h.clients))*spawnStep
			h.clients[conn] = &PlayerState{ID: id, X: x, Y: spawnY}
			log.Printf("Client registered: %v", id)

		case conn := <-h.unregister:
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close()
				log.Printf("Client unregistered: %v", conn.RemoteAddr())
			}

		case ev := <-h.input:
			// Input is the player's current intended direction
			if ps, ok := h.clients[ev.conn]; ok {
				ps.Vx = float64(ev.msg.Dx) * speed
				ps.Vy = float64(ev.msg.Dy) * speed
			}

		case <-ticker.C:
			// Integrate positions.
			for _, ps := range h.clients {
				ps.X += ps.Vx * dt
				ps.Y += ps.Vy * dt
			}

			// Build one snapshot of all players.
			snapshot := SnapshotMsg{Players: make([]PlayerState, 0, len(h.clients))}
			for _, ps := range h.clients {
				snapshot.Players = append(snapshot.Players, *ps)
			}
			data, err := json.Marshal(snapshot)
			if err != nil {
				log.Printf("Failed to marshal snapshot: %v", err)
				continue
			}

			// Push it to everyone.
			for conn := range h.clients {
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					log.Printf("Failed to send snapshot: %v", err)
					conn.Close()
					delete(h.clients, conn)
				}
			}
		}
	}
}
