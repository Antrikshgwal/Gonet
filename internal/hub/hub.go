// hub package owns all authoritative game state and the fixed-tick game loop.
package hub

import (
	"log"
	"log/slog"
	"maps"
	"math"
	"math/rand"
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
	R      float64 `msgpack:"r"`     // current radius; shrinks/grows on collision, 0 while dead
	Score  int     `msgpack:"score"`
	RespawnAt uint32 `msgpack:"respawn"` // tick at which a dead player respawns; 0 if alive

	buf      *InputBuffer `msgpack:"-"` // pending inputs; never serialized
	lastView uint32       `msgpack:"-"` // server tick this player's latest input was based on
}

// Arena bounds and game-mechanic tuning.
const (
	ArenaW = 1000.0
	ArenaH = 600.0

	DefaultRadius = 20.0 // spawn size
	MaxRadius     = 60.0 // growth cap
	LoseRadius    = 6.0  // shrink to this and you lose the round
	DrainRate     = 0.12 // how fast charging transfers radius
	RespawnTicks  = 60   // 3s at 20Hz
)

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// inputEvent ties an input message to the connection that sent it. Private.
type inputEvent struct {
	conn *websocket.Conn
	msg  InputMsg
}

// histFrame is one tick of past positions, kept so collisions can be checked
// against where a player was at the attacker's view time (lag compensation).
// Never serialized — pure server-side state.
type histFrame struct {
	tick uint32
	pos  map[string][2]float64
}

const histLen = 30    // 1.5s of world history at 20Hz
const interpTicks = 3 // clients view remotes ~150ms (3 ticks) in the past

// LobbyPlayer is the public view of a connected player. Served as JSON at
// GET /lobby — an HTTP control-plane endpoint, not the binary game channel.
type LobbyPlayer struct {
	ID    string `json:"id"`
	Score int    `json:"score"`
	Alive bool   `json:"alive"`
}

type lobbyReq struct{ resp chan []LobbyPlayer }

// Hub is the single source of truth for game state.
type Hub struct {
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	input      chan inputEvent
	lobby      chan lobbyReq
	clients    map[*websocket.Conn]*PlayerState
	tick 	   uint32
	hist       []histFrame

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
		lobby:      make(chan lobbyReq),
		clients:    make(map[*websocket.Conn]*PlayerState),
		lastSent:   make(map[*websocket.Conn]map[string]PlayerState),
	}
}

// Lobby returns a snapshot of connected players. Safe to call from HTTP
// handlers — it round-trips through the run loop, which owns the state.
func (h *Hub) Lobby() []LobbyPlayer {
	resp := make(chan []LobbyPlayer)
	h.lobby <- lobbyReq{resp: resp}
	return <-resp
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
			h.clients[conn] = &PlayerState{ID: id, X: x, Y: spawnY, R: DefaultRadius, buf: &InputBuffer{}}
			h.lastSent[conn] = map[string]PlayerState{}
			// Tell the client which player is it, so it can predict and (Day 5)
			// reconcile its own entity. Sent from this goroutine, the only writer.
			if welcome, err := msgpack.Marshal(map[string]any{"you": id}); err == nil {
				conn.WriteMessage(websocket.BinaryMessage, welcome)
			}
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

		case req := <-h.lobby:
			players := make([]LobbyPlayer, 0, len(h.clients))
			for _, ps := range h.clients {
				players = append(players, LobbyPlayer{ID: ps.ID, Score: ps.Score, Alive: ps.RespawnAt == 0})
			}
			req.resp <- players

		case <-ticker.C:
			tickStart := time.Now()
			h.tick++

			// Respawn anyone whose countdown elapsed.
			for _, ps := range h.clients {
				if ps.RespawnAt != 0 && h.tick >= ps.RespawnAt {
					ps.RespawnAt = 0
					ps.R = DefaultRadius
					ps.X = DefaultRadius + rand.Float64()*(ArenaW-2*DefaultRadius)
					ps.Y = DefaultRadius + rand.Float64()*(ArenaH-2*DefaultRadius)
				}
			}

			// One input = one tick-step keeps physics deterministic and
			// replayable, which is what reconciliation depends on. Dead players
			// still drain inputs (so ack_seq advances) but don't move.
			for _, ps := range h.clients {
				for _, in := range ps.buf.Drain() {
					ps.Vx = float64(in.Dx) * speed
					ps.Vy = float64(in.Dy) * speed
					if ps.RespawnAt == 0 {
						ps.X = clamp(ps.X+ps.Vx*dt, ps.R, ArenaW-ps.R)
						ps.Y = clamp(ps.Y+ps.Vy*dt, ps.R, ArenaH-ps.R)
					}
					if in.Seq > ps.AckSeq {
						ps.AckSeq = in.Seq
					}
					ps.lastView = in.Tick // the server tick this input was based on
				}
			}

			h.stepCollisions(dt)
			h.recordHistory()

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


func (h *Hub) stepCollisions(dt float64) {
	players := make([]*PlayerState, 0, len(h.clients))
	for _, ps := range h.clients {
		players = append(players, ps)
	}
	ResolveCollisions(players, h.tick, dt, h.rewind)
}

// rewind returns where id was at or before toTick, from the history ring.
func (h *Hub) rewind(id string, toTick uint32) (float64, float64, bool) {
	for i := len(h.hist) - 1; i >= 0; i-- {
		if h.hist[i].tick <= toTick {
			if p, ok := h.hist[i].pos[id]; ok {
				return p[0], p[1], true
			}
		}
	}
	return 0, 0, false
}

// recordHistory appends the current world positions to the ring buffer so
// later ticks can rewind for lag compensation.
func (h *Hub) recordHistory() {
	frame := histFrame{tick: h.tick, pos: make(map[string][2]float64, len(h.clients))}
	for _, ps := range h.clients {
		frame.pos[ps.ID] = [2]float64{ps.X, ps.Y}
	}
	h.hist = append(h.hist, frame)
	if len(h.hist) > histLen {
		h.hist = h.hist[1:]
	}
}

// ResolveCollisions runs the collision rule over every alive pair. The attacker
// is whoever charges harder along the line between the two; the victim is
// rewound to where the attacker saw it (lag compensation) for the hit test, so
// a charge that connected on the attacker's screen still connects despite lag.
// rewind may be nil (then current positions are used). Pure of hub state so it
// can be tested directly.
func ResolveCollisions(players []*PlayerState, tick uint32, dt float64, rewind func(id string, toTick uint32) (float64, float64, bool)) {
	alive := make([]*PlayerState, 0, len(players))
	for _, ps := range players {
		if ps.RespawnAt == 0 {
			alive = append(alive, ps)
		}
	}

	for i := 0; i < len(alive); i++ {
		for j := i + 1; j < len(alive); j++ {
			a, b := alive[i], alive[j]

			dx, dy := b.X-a.X, b.Y-a.Y
			dist := math.Hypot(dx, dy)
			if dist == 0 {
				continue
			}
			ux, uy := dx/dist, dy/dist
			apprA := a.Vx*ux + a.Vy*uy    // a charging toward b
			apprB := -(b.Vx*ux + b.Vy*uy) // b charging toward a

			attacker, victim, attAppr, vicAppr := a, b, apprA, apprB
			if apprB > apprA {
				attacker, victim, attAppr, vicAppr = b, a, apprB, apprA
			}
			// attAppr<=0 means nobody is closing (separating/stationary). This
			// also stops a pass-through from draining in reverse on the way out.
			if attAppr <= 0 {
				continue
			}

			// Lag compensation: test against where the attacker saw the victim.
			vx, vy := victim.X, victim.Y
			if rewind != nil {
				viewTick := attacker.lastView
				if viewTick > interpTicks {
					viewTick -= interpTicks // clients render remotes ~150ms back too
				}
				if rx, ry, ok := rewind(victim.ID, viewTick); ok {
					vx, vy = rx, ry
				}
			}
			if hx, hy := vx-attacker.X, vy-attacker.Y; math.Hypot(hx, hy) >= attacker.R+victim.R {
				continue // no contact at the attacker's view time
			}

			delta := (attAppr - vicAppr) * DrainRate * dt
			if delta <= 0 {
				continue
			}
			attacker.R = clamp(attacker.R+delta, 0, MaxRadius)
			victim.R = clamp(victim.R-delta, 0, MaxRadius)
			checkLose(victim, attacker, tick)
		}
	}
}

// checkLose retires loser (3s respawn) and credits winner if loser shrank out.
func checkLose(loser, winner *PlayerState, tick uint32) {
	if loser.RespawnAt == 0 && loser.R <= LoseRadius {
		winner.Score++
		loser.R = 0
		loser.RespawnAt = tick + RespawnTicks
	}
}

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
		if !seen || cur.R != prev.R {
			pd["r"] = cur.R
		}
		if !seen || cur.Score != prev.Score {
			pd["score"] = cur.Score
		}
		if !seen || cur.RespawnAt != prev.RespawnAt {
			pd["respawn"] = cur.RespawnAt
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
