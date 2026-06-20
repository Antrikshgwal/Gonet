// hub package owns all authoritative game state and the fixed-tick game loop.
package hub

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
	Seq uint64 `json:"seq"` // monotonic per-client sequence number, used by reconciliation
	Tick uint32 `json:"tick"` // last server tick the client had seen when it sent this
}

const inputBufferSize = 64


// FIFO order at tick time.
type InputBuffer struct {
	buf   [inputBufferSize]InputMsg
	head  int // index of the oldest unread input
	tail  int // index of the next slot to write
	count int // number of unread inputs
}

// Push appends one input. If the buffer is full , it overwrites the oldest input rather than block the tick loop.
func (b *InputBuffer) Push(in InputMsg) {
	b.buf[b.tail] = in
	b.tail = (b.tail + 1) % inputBufferSize
	if b.count == inputBufferSize {
		// Full: the write above clobbered the oldest, so advance head too.
		b.head = (b.head + 1) % inputBufferSize
	} else {
		b.count++
	}
}

// Drain returns all unread inputs in FIFO order and empties the buffer.
func (b *InputBuffer) Drain() []InputMsg {
	out := make([]InputMsg, b.count)
	for i := 0; i < b.count; i++ {
		out[i] = b.buf[(b.head+i)%inputBufferSize]
	}
	b.head, b.tail, b.count = 0, 0, 0
	return out
}

// PlayerState represents the state of a player in the game.
type PlayerState struct {
	ID string  `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	Vx float64 `json:"vx"`
	Vy float64 `json:"vy"`
	AckSeq uint64 `json:"ackseq"` //  the latest input sequence number the server has processed for this player

	buf *InputBuffer // pending inputs; unexported so json.Marshal skips it
}

// SnapshotMsg is the full game state broadcast to all clients each tick.
type SnapshotMsg struct {
	TickID uint32 `json:"tick_id"`
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
	tick 	   uint32
}

// New creates a hub with all channels and the client map initialized.
func New() *Hub {
	return &Hub{
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		input:      make(chan inputEvent),
		clients:    make(map[*websocket.Conn]*PlayerState),
		tick:       0,
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
			h.clients[conn] = &PlayerState{ID: id, X: x, Y: spawnY, buf: &InputBuffer{}}
			log.Printf("Client registered: %v", id)

		case conn := <-h.unregister:
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close()
				log.Printf("Client unregistered: %v", conn.RemoteAddr())
			}

		case ev := <-h.input:
			if ps, ok := h.clients[ev.conn]; ok {
				ps.buf.Push(ev.msg)
			}

		case <-ticker.C:
			h.tick++
			// Drain each player's queued inputs and apply each one as a single
			// tick-step of movement. One input = one step keeps physics
			// deterministic and replayable, which is what reconciliation needs.
			for _, ps := range h.clients {
				for _, in := range ps.buf.Drain() {
					ps.Vx = float64(in.Dx) * speed
					ps.Vy = float64(in.Dy) * speed
					ps.X += ps.Vx * dt
					ps.Y += ps.Vy * dt
					if in.Seq > ps.AckSeq {
						ps.AckSeq = in.Seq
					}
				}
			}

			// Build one snapshot of all players.
			snapshot := SnapshotMsg{TickID: h.tick, Players: make([]PlayerState, 0, len(h.clients))}
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
