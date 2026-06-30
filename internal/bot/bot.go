// Package bot is an autonomous player that speaks the same WebSocket wire
// protocol as the browser. It runs both as the CLI (cmd/bot) and spawned
// in-process by the server for the "play vs bot" button.
package bot

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/Antrikshgwal/gonet/internal/hub"
	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// MLP is the tiny N→H→2 network exported by scripts/train_bot.py.
type MLP struct {
	W1 [][]float64 `json:"w1"`
	B1 []float64   `json:"b1"`
	W2 [][]float64 `json:"w2"`
	B2 []float64   `json:"b2"`
}

// LoadMLP parses an exported model. Returns nil for empty or invalid data, so
// callers fall back to the heuristic.
func LoadMLP(data []byte) *MLP {
	if len(data) == 0 {
		return nil
	}
	m := &MLP{}
	if err := json.Unmarshal(data, m); err != nil || len(m.W1) == 0 {
		return nil
	}
	return m
}

func (m *MLP) forward(x []float64) (int8, int8) {
	hid := make([]float64, len(m.W1))
	for j := range m.W1 {
		s := m.B1[j]
		for k := range m.W1[j] {
			s += m.W1[j][k] * x[k]
		}
		hid[j] = math.Tanh(s)
	}
	var out [2]float64
	for i := range 2 {
		s := m.B2[i]
		for j := range hid {
			s += m.W2[i][j] * hid[j]
		}
		out[i] = math.Tanh(s)
	}
	return axis(out[0]), axis(out[1])
}

// axis snaps a continuous output to one of {-1, 0, 1}.
func axis(v float64) int8 {
	if v > 0.25 {
		return 1
	}
	if v < -0.25 {
		return -1
	}
	return 0
}

// Play connects to wsURL and plays until ctx is cancelled or the connection
// drops. model may be nil (then it uses the chase heuristic). aggro in [0,1]
// tunes skill: low = passive/mills around, high = relentless (but still beatable).
func Play(ctx context.Context, wsURL string, model *MLP, aggro float64) error {
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); c.Close() }() // unblock the read/write on cancel

	var (
		mu       sync.Mutex
		myID     string
		lastTick uint32
		world    = map[string]*hub.PlayerState{}
	)

	go func() { // reader: learn our id, merge snapshot deltas
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				cancel()
				return
			}
			var msg map[string]any
			if msgpack.Unmarshal(data, &msg) != nil {
				continue
			}
			mu.Lock()
			if id, ok := msg["you"].(string); ok {
				myID = id
			}
			if t, ok := msg["tick_id"]; ok {
				lastTick = uint32(toF(t))
			}
			for _, pi := range asSlice(msg["players"]) {
				pm, _ := pi.(map[string]any)
				id, _ := pm["id"].(string)
				p := world[id]
				if p == nil {
					p = &hub.PlayerState{ID: id}
					world[id] = p
				}
				applyDelta(p, pm)
			}
			for _, ri := range asSlice(msg["removed"]) {
				if id, ok := ri.(string); ok {
					delete(world, id)
				}
			}
			mu.Unlock()
		}
	}()

	seq := uint64(1)
	start := time.Now()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			t := time.Since(start).Seconds()
			mu.Lock()
			var dx, dy int8
			self := world[myID]
			opp := nearest(world, myID, self)
			if self != nil && opp != nil && self.RespawnAt == 0 && self.R > 0 {
				if model != nil {
					f := hub.Features(*self, *opp)
					dx, dy = model.forward(f[:])
					if dx == 0 && dy == 0 { // model unsure → don't freeze, fall back
						dx, dy = decide(self, opp, t, aggro)
					}
				} else {
					dx, dy = decide(self, opp, t, aggro)
				}
			}
			tick := lastTick
			mu.Unlock()

			b, _ := msgpack.Marshal(hub.InputMsg{Dx: dx, Dy: dy, Seq: seq, Tick: tick})
			seq++
			if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
				return err
			}
		}
	}
}

// nearest returns the closest live opponent — targeting an *arbitrary* other
// player makes the bot jitter between targets in a crowd (looks broken).
func nearest(world map[string]*hub.PlayerState, myID string, self *hub.PlayerState) *hub.PlayerState {
	if self == nil {
		return nil
	}
	var opp *hub.PlayerState
	best := math.MaxFloat64
	for id, p := range world {
		if id == myID || p.R == 0 {
			continue
		}
		d := (p.X-self.X)*(p.X-self.X) + (p.Y-self.Y)*(p.Y-self.Y)
		if d < best {
			best, opp = d, p
		}
	}
	return opp
}

// decide is a beatable heuristic: the bot reacts every tick (superhuman), so it
// eases off in a rhythm to give a human openings, and retreats when outmatched.
// aggro (0..1) sets how much it idles — low bots mill around, high bots press.
func decide(self, opp *hub.PlayerState, t, aggro float64) (int8, int8) {
	if math.Sin(t*1.6+aggro*9) < 0.5-1.1*aggro { // ease off; passive bots idle more
		return 0, 0
	}
	dx, dy := opp.X-self.X, opp.Y-self.Y
	if self.R+4 < opp.R { // outmatched → back off, don't feed radius
		return sign(-dx, 6), sign(-dy, 6)
	}
	return sign(dx, 6), sign(dy, 6)
}

func sign(d, dead float64) int8 {
	if d > dead {
		return 1
	}
	if d < -dead {
		return -1
	}
	return 0
}

func applyDelta(p *hub.PlayerState, m map[string]any) {
	if v, ok := m["x"]; ok {
		p.X = toF(v)
	}
	if v, ok := m["y"]; ok {
		p.Y = toF(v)
	}
	if v, ok := m["vx"]; ok {
		p.Vx = toF(v)
	}
	if v, ok := m["vy"]; ok {
		p.Vy = toF(v)
	}
	if v, ok := m["r"]; ok {
		p.R = toF(v)
	}
	if v, ok := m["respawn"]; ok {
		p.RespawnAt = uint32(toF(v))
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func toF(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int8:
		return float64(n)
	case uint8:
		return float64(n)
	case int16:
		return float64(n)
	case uint16:
		return float64(n)
	case int32:
		return float64(n)
	case uint32:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return 0
}
