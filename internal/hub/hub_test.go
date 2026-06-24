package hub

import (
	"encoding/json"
	"math"
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

func TestCollisionChargerDrainsStationary(t *testing.T) {
	// a charges right into a stationary b; they overlap (centers 30 apart, radii 20+20).
	a := &PlayerState{ID: "a", X: 100, Y: 100, Vx: 200, R: 20}
	b := &PlayerState{ID: "b", X: 130, Y: 100, R: 20}
	ResolveCollisions([]*PlayerState{a, b}, 1, 0.05, nil)

	if a.R <= 20 || b.R >= 20 {
		t.Fatalf("charger should grow and target shrink: a.R=%.3f b.R=%.3f", a.R, b.R)
	}
	if delta := (a.R - 20) - (20 - b.R); math.Abs(delta) > 1e-9 {
		t.Errorf("radius transfer should be conserved, off by %v", delta)
	}
}

func TestCollisionLoseAndRespawn(t *testing.T) {
	a := &PlayerState{ID: "a", X: 100, Y: 100, Vx: 200, R: 20}
	b := &PlayerState{ID: "b", X: 120, Y: 100, R: LoseRadius + 0.01} // one hit from out
	ResolveCollisions([]*PlayerState{a, b}, 500, 0.05, nil)

	if b.RespawnAt != 500+RespawnTicks {
		t.Errorf("loser should be scheduled to respawn, got RespawnAt=%d", b.RespawnAt)
	}
	if b.R != 0 {
		t.Errorf("dead player radius should be 0, got %.3f", b.R)
	}
	if a.Score != 1 {
		t.Errorf("winner should score, got %d", a.Score)
	}
}

func TestCollisionPassThroughNetDrains(t *testing.T) {
	// a charges fully through a stationary b; b must end up smaller, not bounce
	// back to its starting size once a exits the far side.
	a := &PlayerState{ID: "a", X: 100, Y: 200, Vx: 200, R: 20}
	b := &PlayerState{ID: "b", X: 200, Y: 200, R: 20}
	for range 16 {
		a.X += a.Vx * 0.05 // 10px/tick, server-identical
		ResolveCollisions([]*PlayerState{a, b}, 1, 0.05, nil)
	}
	if b.R >= 20 {
		t.Fatalf("victim should net-shrink after a full pass, ended at %.2f", b.R)
	}
	if d := (a.R - 20) - (20 - b.R); math.Abs(d) > 1e-9 {
		t.Errorf("transfer not conserved, off by %v", d)
	}
}

func TestCollisionLagCompensation(t *testing.T) {
	rewind := func(id string, toTick uint32) (float64, float64, bool) {
		if id == "b" {
			return 125, 100, true // where the attacker saw b: in contact
		}
		return 0, 0, false
	}
	a := &PlayerState{ID: "a", X: 100, Y: 100, Vx: 200, R: 20, lastView: 100}
	b := &PlayerState{ID: "b", X: 300, Y: 100, R: 20}
	ResolveCollisions([]*PlayerState{a, b}, 1, 0.05, rewind)
	if b.R >= 20 {
		t.Fatalf("hit should register against the rewound position, b.R=%.2f", b.R)
	}

	// Same geometr: current positions are too far apart → no hit.
	a2 := &PlayerState{ID: "a", X: 100, Y: 100, Vx: 200, R: 20, lastView: 100}
	b2 := &PlayerState{ID: "b", X: 300, Y: 100, R: 20}
	ResolveCollisions([]*PlayerState{a2, b2}, 1, 0.05, nil)
	if b2.R != 20 {
		t.Errorf("without rewind the far victim should be untouched, b.R=%.2f", b2.R)
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
