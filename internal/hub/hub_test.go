package hub

import (
	"encoding/json"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func twoPlayerWorld() map[string]PlayerState {
	return map[string]PlayerState{
		"10.0.0.1:5001": {ID: "10.0.0.1:5001", X: 312.5, Y: 188.0, Vx: 200, Vy: 0, AckSeq: 1280},
		"10.0.0.2:5002": {ID: "10.0.0.2:5002", X: 287.0, Y: 204.5, Vx: 0, Vy: -200, AckSeq: 1280},
	}
}

func fullWire(world map[string]PlayerState, tick uint32) map[string]any {
	players := make([]map[string]any, 0, len(world))
	for _, p := range world {
		players = append(players, map[string]any{
			"id": p.ID, "x": p.X, "y": p.Y, "vx": p.Vx, "vy": p.Vy, "ackseq": p.AckSeq,
		})
	}
	return map[string]any{"tick_id": tick, "players": players}
}

// TestWireSizes reports encoded frame sizes for a 2-player world. Run with
// `go test -run TestWireSizes -v ./internal/hub` to see the numbers.
func TestWireSizes(t *testing.T) {
	base := twoPlayerWorld()

	full := fullWire(base, 1281)
	jsonB, _ := json.Marshal(full)
	mpFull, _ := msgpack.Marshal(full)

	// Both players are active (ackseq advances every tick); only player 1 moves.
	moving := twoPlayerWorld()
	for id, ps := range moving {
		ps.AckSeq = 1281
		moving[id] = ps
	}
	p := moving["10.0.0.1:5001"]
	p.X, p.Vx = 322.5, 200
	moving["10.0.0.1:5001"] = p
	dMove, _ := ComputeDelta(1281, moving, base)
	mpMove, _ := msgpack.Marshal(dMove)

	// Steady state: both active, nobody moving — only ackseq changes.
	idle := twoPlayerWorld()
	for id, ps := range idle {
		ps.AckSeq = 1281
		idle[id] = ps
	}
	dIdle, _ := ComputeDelta(1281, idle, base)
	mpIdle, _ := msgpack.Marshal(dIdle)

	t.Logf("JSON full snapshot:        %d B", len(jsonB))
	t.Logf("msgpack full snapshot:     %d B", len(mpFull))
	t.Logf("msgpack delta (1 moving):  %d B", len(mpMove))
	t.Logf("msgpack delta (no motion): %d B", len(mpIdle))
}

func TestComputeDeltaOmitsUnchangedFields(t *testing.T) {
	last := twoPlayerWorld()
	current := twoPlayerWorld()
	p := current["10.0.0.1:5001"]
	p.X += 5
	current["10.0.0.1:5001"] = p

	delta, next := ComputeDelta(9, current, last)
	players, ok := delta["players"].([]map[string]any)
	if !ok || len(players) != 1 {
		t.Fatalf("expected exactly 1 changed player, got %v", delta["players"])
	}
	pd := players[0]
	if _, sent := pd["y"]; sent {
		t.Error("y was unchanged but included in delta")
	}
	if _, sent := pd["x"]; !sent {
		t.Error("x changed but was not included in delta")
	}
	if next["10.0.0.1:5001"].X != current["10.0.0.1:5001"].X {
		t.Error("baseline was not advanced to current state")
	}
}

func TestComputeDeltaRemoval(t *testing.T) {
	last := twoPlayerWorld()
	delta, _ := ComputeDelta(1, map[string]PlayerState{}, last)
	removed, _ := delta["removed"].([]string)
	if len(removed) != 2 {
		t.Fatalf("expected both players removed, got %v", delta["removed"])
	}
	if _, ok := delta["players"]; ok {
		t.Error("no players remain, players key should be omitted")
	}
}

func BenchmarkComputeDelta(b *testing.B) {
	current := twoPlayerWorld()
	last := twoPlayerWorld()
	b.ReportAllocs()
	for b.Loop() {
		ComputeDelta(1, current, last)
	}
}

func BenchmarkMarshalFullSnapshot(b *testing.B) {
	full := fullWire(twoPlayerWorld(), 1)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = msgpack.Marshal(full)
	}
}
