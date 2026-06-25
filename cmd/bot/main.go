// Command bot is an autonomous player. It speaks the exact same wire protocol
// as the browser client. It
// acts via a trained behavior-cloning MLP (-model) or a chase heuristic.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"github.com/Antrikshgwal/gonet/internal/hub"
	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// mlp is a tiny 6 -> H -> 2 network exported by scripts/train_bot.py.
type mlp struct {
	W1 [][]float64 `json:"w1"` // H x 6
	B1 []float64   `json:"b1"` // H
	W2 [][]float64 `json:"w2"` // 2 x H
	B2 []float64   `json:"b2"` // 2
}

func (m *mlp) forward(x []float64) (int8, int8) {
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

func main() {
	addr := flag.String("addr", "ws://127.0.0.1:8080/ws", "server websocket URL")
	modelPath := flag.String("model", "", "behavior-cloning model JSON (omit for chase heuristic)")
	flag.Parse()

	var model *mlp
	if *modelPath != "" {
		raw, err := os.ReadFile(*modelPath)
		if err != nil {
			log.Fatalf("read model: %v", err)
		}
		model = &mlp{}
		if err := json.Unmarshal(raw, model); err != nil {
			log.Fatalf("parse model: %v", err)
		}
		log.Printf("loaded model %s (hidden=%d)", *modelPath, len(model.W1))
	} else {
		log.Print("no model — using chase heuristic")
	}

	c, _, err := websocket.DefaultDialer.Dial(*addr, nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()

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
				log.Printf("read: %v", err)
				os.Exit(0)
			}
			var m map[string]any
			if msgpack.Unmarshal(data, &m) != nil {
				continue
			}
			mu.Lock()
			if id, ok := m["you"].(string); ok {
				myID = id
			}
			if t, ok := m["tick_id"]; ok {
				lastTick = uint32(toF(t))
			}
			for _, pi := range asSlice(m["players"]) {
				pm, _ := pi.(map[string]any)
				id, _ := pm["id"].(string)
				p := world[id]
				if p == nil {
					p = &hub.PlayerState{ID: id}
					world[id] = p
				}
				applyDelta(p, pm)
			}
			for _, ri := range asSlice(m["removed"]) {
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
	for range ticker.C {
		t := time.Since(start).Seconds()
		mu.Lock()
		var dx, dy int8
		self := world[myID]
		var opp *hub.PlayerState
		for id, p := range world {
			if id != myID {
				opp = p
			}
		}
		if self != nil && opp != nil && self.RespawnAt == 0 && self.R > 0 {
			if model != nil {
				f := hub.Features(*self, *opp)
				dx, dy = model.forward(f[:])
				if dx == 0 && dy == 0 { // model unsure → don't freeze, fall back
					dx, dy = decide(self, opp, t)
				}
			} else {
				dx, dy = decide(self, opp, t)
			}
		}
		tick := lastTick
		mu.Unlock()

		b, _ := msgpack.Marshal(hub.InputMsg{Dx: dx, Dy: dy, Seq: seq, Tick: tick})
		seq++
		if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
			return
		}
	}
}

func decide(self, opp *hub.PlayerState, t float64) (int8, int8) {
	dx, dy := opp.X-self.X, opp.Y-self.Y
	if math.Sin(t*1.6) < -0.25 { // ~1/3 of the time, stop pressing
		return 0, 0
	}
	if self.R+3 < opp.R { // outmatched → back off, don't hand over radius
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
