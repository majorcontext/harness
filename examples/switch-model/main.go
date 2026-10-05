// Switch-model starts a conversation on one model and continues it on
// another. History is provider-neutral, so the second model sees the whole
// conversation with no migration.
//
//	ANTHROPIC_API_KEY=... go run ./examples/switch-model
//	ANTHROPIC_API_KEY=... OPENAI_API_KEY=... go run ./examples/switch-model -then openai/gpt-5
//	OPENAI_API_KEY=... OPENROUTER_API_KEY=... go run ./examples/switch-model -first openai/gpt-5 -then openrouter/anthropic/claude-sonnet-5
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

func main() {
	first := flag.String("first", config.DefaultModel, "model for the first prompt")
	then := flag.String("then", "anthropic/claude-haiku-4-5-20251001", "model for the second prompt")
	flag.Parse()
	ctx := context.Background()

	rt, err := harness.New(harness.Options{Store: harness.NewMemStore()})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close(ctx) }()
	s, err := rt.Create(ctx, protocol.CreateSession{Model: *first})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("[%s]\n", s.View().Model)
	say(ctx, s, "first", "Pick a random animal. Reply with only its name.")

	if _, err := s.Update(ctx, protocol.SettingsPatch{Model: then}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n\n[%s]\n", s.View().Model)
	say(ctx, s, "second", "Which animal did you pick? Give one fact about it.")
	fmt.Println()
}

// say sends text and streams the reply until the turn ends.
func say(ctx context.Context, s *harness.Session, id, text string) {
	head := s.View().HeadSeq
	if _, err := s.Submit(ctx, protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}}); err != nil {
		log.Fatal(err)
	}
	for e, err := range s.Events(ctx, head) {
		if err != nil {
			log.Fatal(err)
		}
		switch e.Kind {
		case protocol.KindItemDelta:
			var f protocol.ItemFrame
			if json.Unmarshal(e.Data, &f) == nil && f.Type == "text" {
				fmt.Print(f.Text)
			}
		case "turn.ended":
			return
		}
	}
}
