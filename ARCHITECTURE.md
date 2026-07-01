# Architecture

Gonet is a multi-player real-time game built with Go.
The game is two circles in an arena; the game comes with serious netcode so it
feels instant and fair despite lag, jitter, and divergence. This document is the
map: the concurrency model, the wire protocol, and the six-stage pipeline a
single keypress travels through.

## Topology

<img src="assets/Topology.png" alt="Topology: a single hub goroutine owns all state, with one goroutine per connection and channels at the edges" width="720">


A `cmd/bot` client and the `cmd/loadtest` harness connect over the *same*
WebSocket protocol as the browser.

## Concurrency model: one owner, channels at the edges

All game state lives in a single goroutine (`hub.Run`). Nothing else touches it.

- **Per connection:** one goroutine blocks on `conn.ReadMessage()`, decodes an
  `InputMsg`, and hands it to the hub over the `input` channel. It never mutates
  state.
- **The hub goroutine** owns the `clients` map, the tick counter, history, and
  every player's position. It `select`s over `register`, `unregister`, `input`,
  `lobby`, and the 50 ms `ticker`. Because it's the only writer, **there are no
  mutexes on game state** — the channel hand-off is the synchronization.
- **Reads from outside** (the `GET /lobby` HTTP handler) round-trip a request
  through the hub over a channel and get a snapshot back, so even read access
  never races.

This is the Go "share memory by communicating" pattern: serialize all state
access through one goroutine instead of locking shared structures.

## Fixed-timestep tick loop

The simulation advances on a `time.NewTicker(50ms)` — **20 ticks per second** —
independent of how fast inputs or connections arrive. Each tick:

1. respawn anyone whose 3s countdown elapsed,
2. drain each player's input buffer and apply one movement step per input,
3. resolve collisions,
4. snapshot positions into the history ring (for lag compensation),
5. build and send a per-client delta,
6. log `tick_id / players / snapshot_bytes / tick_ms` (sampled at ~1 Hz).

A fixed delta keeps physics **deterministic** — the same inputs always produce
the same positions — which is the precondition for client-side prediction and
reconciliation to agree with the server.

## Wire protocol

MessagePack (binary) over WebSocket. Two messages, plus a one-time welcome.

| Direction | Message | Fields |
|---|---|---|
| client → server | `InputMsg` | `dx, dy` (−1/0/1) · `seq` (monotonic) · `tick` (last server tick seen) |
| server → client | snapshot delta | `tick_id` · `players: [changed fields only]` · `removed: [ids]` |
| server → client | welcome (on connect) | `{ you: <your id> }` |

**Deltas, not full state.** The server keeps the last snapshot it sent *each*
client and transmits only fields that changed since. Idle players vanish from
the wire; the client merges each delta onto a persistent world map. The welcome
message tells a client which entity is *itself*, so it can predict and reconcile
that one.

## Whole keypress pipeline

<img src="assets/pipeline.png" alt="Pipeline: Predict → Send → Simulate → Snapshot → Reconcile → Interpolate" width="720">

### 1 · Predict (client)
Pressing a key applies the move to a local copy (`localState`) **immediately**,
running the same physics the server will. No round-trip, so your own dot is
zero-latency. This is what cancels ping.

### 2 · Send (client)
The same input goes to the server stamped with a monotonic `seq` and the last
server `tick` you'd seen (used later for lag compensation). `seq` starts at 1 so
the server's initial `ack_seq` of 0 unambiguously means "nothing acked yet."

### 3 · Simulate (server, authoritative)
Inputs land in a 64-slot ring buffer per player. At tick time the hub drains the
buffer and applies **one movement step per input**, clamped to the arena. The
highest `seq` it consumed becomes that player's `ack_seq`. The server is the
single source of truth; the client's prediction is only a guess until confirmed.

### 4 · Snapshot (server → client)
After stepping, the hub computes each client's delta against what it last sent
them and broadcasts it. `ack_seq` rides along so the client knows how far the
server has caught up.

### 5 · Reconcile (client)
On each snapshot the client **snaps** its own dot to the authoritative position
(which reflects inputs up to `ack_seq`), then **replays every still-unacked
input** (`seq > ack_seq`) on top. With identical deterministic physics this
reproduces the predicted position exactly when the prediction was right, and
silently corrects it when it wasn't — no rubber-banding. The HUD's `corr` shows
the magnitude of each correction (≈0 in steady state).

### 6 · Interpolate (client, remote players)
You can't predict opponents — you don't know their inputs. Instead the client
buffers timestamped position samples and renders remote players at
**now − 150 ms**, lerping between the two samples bracketing that time. The
150 ms cushion absorbs jitter; snapshots arriving unevenly still produce smooth
motion. If render time runs past the newest sample (a gap), it dead-reckons from
the last velocity, capped. The trade is deliberate: you see opponents slightly
in the past so they always move smoothly.

> **The split is about information, not delay:** you *predict* what you control
> (you know your inputs) and *interpolate* what you don't (you only know past
> positions). Prediction fights latency; interpolation fights jitter.

## Lag compensation

A relentless charge that connected on *your* laggy screen should still land. The
hub keeps a 30-frame ring of past positions (1.5 s). When resolving a collision,
the attacker is whoever charges harder along the line between the two; the victim
is **rewound** to where the attacker saw it (`attacker.lastView − interpTicks`)
for the hit test. So the fairness is judged against what the acting player
actually saw, the way an FPS confirms shots server-side.

## Game mechanics

Overlapping circles transfer radius from the player charging in less to the one
charging in more (closing-speed along the contact line). Shrink to the lose
radius and you're out: the opponent scores, you vanish for 3 s, then respawn at
a random spot at full size. All authoritative — the client renders radius/score
from snapshots and predicts only its own *position*.

## The bot — just another client

`cmd/bot` connects over the same WebSocket and speaks the same `InputMsg`/
snapshot protocol; the server can't distinguish it from a human. It plays via a
behavior-cloning MLP or a chase heuristic:

```
RECORD=games.jsonl go run ./cmd/server   # log (state, action) pairs while you play
python scripts/train_bot.py games.jsonl bot_model.json   # fit a 4→16→2 numpy MLP
go run ./cmd/bot -model bot_model.json   # the bot reads snapshots, runs a
                                         # forward pass, sends InputMsg
```

Features are deliberately **positional** (dx/dy to opponent + both radii, no own
velocity) — feeding velocity back in makes the clone copy momentum and stick to
walls. The key insight: behavior cloning imitates the demonstrator, so the bot
approaches "plays like you" but can't exceed you; beating that ceiling needs
self-play RL.

**In production the bot is server-managed.** There are two isolated arenas — a
PvP hub (real players only) and a practice hub — so nobody can inject bots into
someone's real match. When a human joins practice (`/play?mode=practice`), the
server maintains a shifting population of in-process bots (each an ordinary
loopback WebSocket client) with randomized skill (`aggro`) and lifetimes, so it
feels like a live lobby; they stop spawning once the arena empties. Each bot
targets its **nearest** opponent — so in a crowd they fight each other rather
than gang up on the human — eases off in a rhythm, and retreats when outmatched,
which keeps it beatable.

## Scaling and known limits

Measured with `cmd/loadtest` (N concurrent headless clients in one arena):

| Load | tick time | notes |
|---|---|---|
| 40 clients | 2–4 ms | ~12× under the 50 ms budget |
| 150 clients | 23–45 ms | at the budget knee |

The bottleneck is **O(n²)**: in a single shared arena, both collision resolution
and per-client delta encoding scale with players². The architecture's answer is
sharding into 2-player **rooms** — each room is O(1) per session, so the server
scales to thousands of independent games. The single-arena numbers are a
deliberate worst case. `pprof` (localhost:6060) confirms goroutines return to
baseline after load — no leak.

## Benchmarks

Measured on a 2-player world (`go test -bench . -benchmem ./internal/hub`,
11th-gen i5-11400H).

Wire size per snapshot:

| Encoding | Bytes | vs JSON full |
|---|--:|--:|
| JSON, full snapshot | 171 | — |
| MessagePack, full snapshot | 183 | +7 % |
| MessagePack delta, one player moving | 102 | −40 % |
| MessagePack delta, steady state | 91 | −47 % |

MessagePack alone is a wash here (string keys + `float64`s dominate); the win is
delta encoding, which roughly halves steady-state bandwidth.

Tick processing:

| Operation | Time | Allocs/op |
|---|--:|--:|
| `ComputeDelta` (2 players) | ~0.8 µs | 11 |
| Marshal full snapshot | ~1.0 µs | 6 |

A 50 ms tick budget against single-digit-µs work is ~4 orders of magnitude of
headroom.

## Running & verifying locally

```bash
go run ./cmd/server        # http://localhost:8080  (/play · /play?mode=practice)
```

Every netcode concept is observable live in the client's HUD:

| Feature | How to see it |
|---|---|
| prediction | `/play?lag=200` — your dot is instant; the amber server ghost trails (`drift`) |
| reconciliation | `/play?lag=500` — no rubber-banding; `corr` spikes only on collisions/respawns |
| interpolation | two tabs, one `/play?jitter=100` — the other player stays smooth |
| lag compensation | `go test -run TestCollisionLagCompensation -v ./internal/hub` |
| delta bandwidth | DevTools → Network → WS → Messages — frame sizes shrink when idle |

Tests, benchmarks, load test, and profiling:

```bash
go test ./...
go test -bench . -benchmem ./internal/hub
go run ./cmd/loadtest -clients 150 -dur 10s        # watch tick_ms (MAX_PLAYERS caps at 200)
go tool pprof localhost:6060/debug/pprof/profile?seconds=10
```

Training clone (Python + numpy):

```bash
RECORD=games.jsonl go run ./cmd/server              # play a few minutes; appends across runs
python scripts/train_bot.py games.jsonl bot_model.json
go run ./cmd/bot -model bot_model.json              # or -aggro 0.4, or -model none for heuristic
```

## Deploy

One static binary, all assets embedded; it listens on `$PORT` (or `:8080`), so
any Docker host works. Free option is **Render**: push to GitHub, then
**New → Blueprint** (it reads `render.yaml`). HTTPS is enforced and the client
switches to `wss://` on its own. `fly.toml` is also included (Fly is paid).

## Repository layout

```
cmd/server     main.go — wires the hubs, HTTP routes, embedded static files
cmd/bot        headless WebSocket player (MLP or heuristic)
cmd/loadtest   concurrent-client load harness
internal/hub   tick loop, physics, collisions, lag comp, delta encoding
client/        the game UI (prediction, reconciliation, interpolation, Canvas)
site/          the landing page (served at /, game at /play)
scripts/       train_bot.py — behavior-cloning trainer
```
