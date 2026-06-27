// Command loadtest hammers the server with many concurrent WebSocket clients,
// each behaving like a real player (20Hz inputs, draining snapshots). It reports
// client-side throughput; watch the server's own tick_ms log for the headline
// "p99 tick time under load" number.
package main

import (
	"flag"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Antrikshgwal/gonet/internal/hub"
	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

func main() {
	clients := flag.Int("clients", 40, "concurrent clients (all share one arena)")
	dur := flag.Duration("dur", 10*time.Second, "test duration")
	addr := flag.String("addr", "ws://127.0.0.1:8080/ws", "server websocket URL")
	flag.Parse()

	var sent, recv, errs int64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < *clients; i++ {
		wg.Go(func() {
			c, _, err := websocket.DefaultDialer.Dial(*addr, nil)
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			defer c.Close()

			go func() { // drain inbound snapshots
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
					atomic.AddInt64(&recv, 1)
				}
			}()

			seq := uint64(1)
			t := time.NewTicker(50 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					b, _ := msgpack.Marshal(hub.InputMsg{
						Dx: int8(rand.Intn(3) - 1), Dy: int8(rand.Intn(3) - 1), Seq: seq,
					})
					seq++
					if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
						atomic.AddInt64(&errs, 1)
						return
					}
					atomic.AddInt64(&sent, 1)
				}
			}
		})
		time.Sleep(5 * time.Millisecond) // don't thundering-herd
	}

	log.Printf("%d clients connecting for %s...", *clients, *dur)
	time.Sleep(*dur)
	close(stop)
	wg.Wait()

	secs := dur.Seconds()
	log.Printf("done | sent=%d recv=%d errs=%d | %.0f inputs/s, %.0f snapshots/s",
		sent, recv, errs, float64(sent)/secs, float64(recv)/secs)
}
