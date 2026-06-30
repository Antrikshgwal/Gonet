// Command bot is an autonomous player — it speaks the same WebSocket protocol
// as the browser. By default it uses the embedded behavior-cloning model;
// -model loads a different one, and -model none forces the chase heuristic.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	gonet "github.com/Antrikshgwal/gonet"
	"github.com/Antrikshgwal/gonet/internal/bot"
)

func main() {
	addr := flag.String("addr", "ws://127.0.0.1:8080/ws", "server websocket URL")
	modelPath := flag.String("model", "", "model JSON path; empty = embedded default; 'none' = heuristic")
	flag.Parse()

	var model *bot.MLP
	switch *modelPath {
	case "none":
		log.Print("chase heuristic")
	case "":
		if model = bot.LoadMLP(gonet.BotModel); model != nil {
			log.Print("using embedded model")
		} else {
			log.Print("no embedded model — chase heuristic")
		}
	default:
		raw, err := os.ReadFile(*modelPath)
		if err != nil {
			log.Fatalf("read model: %v", err)
		}
		if model = bot.LoadMLP(raw); model == nil {
			log.Fatalf("invalid model: %s", *modelPath)
		}
		log.Printf("loaded model %s", *modelPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := bot.Play(ctx, *addr, model); err != nil {
		log.Printf("bot exited: %v", err)
	}
}
