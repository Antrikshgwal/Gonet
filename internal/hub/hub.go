// hub package owns all authoritative game state and the fixed-tick game loop.
package hub

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// InputMsg is a movement input sent by a client. Exported so other packages
// (e.g. the bot client) can build and send the same wire format.
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

// SnapshotMsg is the full game state broadcast to all clients each tick.
type SnapshotMsg struct {
	Players []PlayerState `json:"players"`
}

// inputEvent ties an input message to the connection that sent it. Private.
type inputEvent struct {
	conn *websocket.Conn
	msg  InputMsg
}

// Hub is the single source of truth for game state.
type Hub struct {
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	input      chan inputEvent
	clients    map[*websocket.Conn]*PlayerState
}

// New creates a hub with all channels and the client map initialized.
func New() *Hub {
	return &Hub{
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		input:      make(chan inputEvent),
		clients:    make(map[*websocket.Conn]*PlayerState),
	}
}

// Register adds a connection as a new player. Blocks until the run loop
// accepts it.
func (h *Hub) Register(conn *websocket.Conn) { h.register <- conn }

// Unregister removes a connection
func (h *Hub) Unregister(conn *websocket.Conn) { h.unregister <- conn }

// SendInput forwards a client's input to the run loop.
func (h *Hub) SendInput(conn *websocket.Conn, msg InputMsg) {
	h.input <- inputEvent{conn: conn, msg: msg}
}

// Run drives the game loop. Call it once in its own goroutine; it owns all
// state mutation
func (h *Hub) Run() {
	// speed is how fast players move, in units per second.
	const speed = 200.0
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
			// Input is the player's current intended direction, not a delta.
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
