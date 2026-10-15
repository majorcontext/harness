// Switch-model starts a conversation on one model and continues it on
// another. History is provider-neutral, so the second model sees the whole
// conversation with no migration.
//
//	go run ./examples/switch-model
//	go run ./examples/switch-model -first codex/gpt-6.1-sol -then claude-code/sonnet
//
// With no flags, the first model is the default of the machine: the API
// default when ANTHROPIC_API_KEY is set, else the ChatGPT login of "harness
// login codex", else the claude program on PATH. The second model is the first
// other model that can run: the Codex login, the claude program, or the API
// haiku model with ANTHROPIC_API_KEY. When there is none, the example says so
// and exits.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/examples/internal/login"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func main() {
	first := flag.String("first", "", "model for the first prompt; empty selects the default")
	then := flag.String("then", "", "model for the second prompt; empty selects another model that can run")
	flag.Parse()
	ctx := context.Background()

	rt, err := harness.New(harness.Options{Store: store.NewMemStore(), Config: login.Config(), Getenv: os.Getenv})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close(ctx) }()
	s, err := rt.Create(ctx, protocol.CreateSession{Model: *first})
	if err != nil {
		log.Fatal(err)
	}

	current := s.Snapshot().Model
	if *then == "" {
		alts := login.Alternatives(current)
		if len(alts) == 0 {
			fmt.Printf("Only %s can run here, so there is no model to switch to. Run \"harness login codex\", install claude, or set ANTHROPIC_API_KEY.\n", current)
			return
		}
		*then = alts[0]
	}
	fmt.Printf("[%s]\n", current)
	say(ctx, s, "first", "Pick a random animal. Reply with only its name.")

	if _, err := s.Update(ctx, protocol.SettingsPatch{Model: then}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n\n[%s]\n", s.Snapshot().Model)
	say(ctx, s, "second", "Which animal did you pick? Give one fact about it.")
	fmt.Println()
}

// say sends text, streams the reply, and exits when the turn fails.
func say(ctx context.Context, s *harness.Session, id, text string) {
	t, err := s.Prompt(ctx, protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}})
	if err != nil {
		log.Fatal(err)
	}
	for e, err := range t.Events(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		switch e.Kind {
		case protocol.KindItemDelta:
			var f protocol.ItemFrame
			if json.Unmarshal(e.Data, &f) == nil && f.Type == "text" {
				fmt.Print(f.Text)
			}
		}
	}
	res, err := t.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if res.StopReason == protocol.StopFailed {
		log.Fatal(res.Error)
	}
}
