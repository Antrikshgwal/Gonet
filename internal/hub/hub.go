// hub package owns all authoritative game state and the fixed-tick game loop.
package hub

import (
	"log"
	"log/slog"
	"maps"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// InputMsg is a movement input sent by a client.
type InputMsg struct {
	Dx int8 `msgpack:"dx"`
	Dy int8 `msgpack:"dy"`
	Seq uint64 `msgpack:"seq"` // monotonic per-client sequence number, used by reconciliation
	Tick uint32 `msgpack:"tick"` // last server tick the client had seen when it sent this
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
	ID string  `msgpack:"id"`
	X  float64 `msgpack:"x"`
	Y  float64 `msgpack:"y"`
	Vx float64 `msgpack:"vx"`
	Vy float64 `msgpack:"vy"`
	AckSeq uint64 `msgpack:"ackseq"` // latest input seq the server has simulated for this player

	buf *InputBuffer `msgpack:"-"` // pending inputs; never serialized
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

	// lastSent is the world each client was last told about, keyed by player id.
	// Deltas are computed against it so we only resend fields that changed.
	lastSent map[*websocket.Conn]map[string]PlayerState
}

// New creates a hub with all channels and the client map initialized.
func New() *Hub {
	return &Hub{
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		input:      make(chan inputEvent),
		clients:    make(map[*websocket.Conn]*PlayerState),
		lastSent:   make(map[*websocket.Conn]map[string]PlayerState),
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
			h.lastSent[conn] = map[string]PlayerState{}
			log.Printf("Client registered: %v", id)

		case conn := <-h.unregister:
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				delete(h.lastSent, conn)
				conn.Close()
				log.Printf("Client unregistered: %v", conn.RemoteAddr())
			}

		case ev := <-h.input:
			if ps, ok := h.clients[ev.conn]; ok {
				ps.buf.Push(ev.msg)
			}

		case <-ticker.C:
			tickStart := time.Now()
			h.tick++

			// One input = one tick-step keeps physics deterministic and
			// replayable, which is what reconciliation depends on.
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

			current := make(map[string]PlayerState, len(h.clients))
			for _, ps := range h.clients {
				current[ps.ID] = *ps
			}

			totalBytes := 0
			for conn := range h.clients {
				data := h.buildDelta(conn, current)
				if data == nil {
					continue
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
					conn.Close()
					delete(h.clients, conn)
					delete(h.lastSent, conn)
					continue
				}
				totalBytes += len(data)
			}

			elapsed := time.Since(tickStart)
			if h.tick%20 == 0 { // sample at ~1 Hz instead of flooding at 20 Hz
				slog.Info("tick", "tick_id", h.tick, "players", len(current),
					"snapshot_bytes", totalBytes, "tick_ms", elapsed.Milliseconds())
			}
			if elapsed > 40*time.Millisecond {
				slog.Warn("slow tick", "tick_id", h.tick, "tick_ms", elapsed.Milliseconds())
			}
		}
	}
}

// buildDelta computes this client's delta against its lastSent record, updates
// that record, and returns the encoded frame. Returns nil on a marshal error.
func (h *Hub) buildDelta(conn *websocket.Conn, current map[string]PlayerState) []byte {
	delta, next := ComputeDelta(h.tick, current, h.lastSent[conn])
	h.lastSent[conn] = next

	data, err := msgpack.Marshal(delta)
	if err != nil {
		log.Printf("marshal delta: %v", err)
		return nil
	}
	return data
}

// ComputeDelta returns the wire payload carrying only the fields that changed
// from last to current, plus the ids that disappeared under "removed". next is
// the snapshot to remember as the new baseline. Pure (no I/O) so it can be
// tested directly.
func ComputeDelta(tick uint32, current, last map[string]PlayerState) (map[string]any, map[string]PlayerState) {
	players := make([]map[string]any, 0, len(current))
	for id, cur := range current {
		prev, seen := last[id]
		pd := map[string]any{"id": id}
		if !seen || cur.X != prev.X {
			pd["x"] = cur.X
		}
		if !seen || cur.Y != prev.Y {
			pd["y"] = cur.Y
		}
		if !seen || cur.Vx != prev.Vx {
			pd["vx"] = cur.Vx
		}
		if !seen || cur.Vy != prev.Vy {
			pd["vy"] = cur.Vy
		}
		if !seen || cur.AckSeq != prev.AckSeq {
			pd["ackseq"] = cur.AckSeq
		}
		if len(pd) > 1 { // more than just "id" → something changed
			players = append(players, pd)
		}
	}

	var removed []string
	for id := range last {
		if _, ok := current[id]; !ok {
			removed = append(removed, id)
		}
	}

	delta := map[string]any{"tick_id": tick}
	if len(players) > 0 {
		delta["players"] = players
	}
	if len(removed) > 0 {
		delta["removed"] = removed
	}

	next := make(map[string]PlayerState, len(current))
	maps.Copy(next, current)
	return delta, next
}
