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
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
	"github.com/majorcontext/harness/provider/openai"
	"github.com/majorcontext/harness/provider/openaicompat"
)

func main() {
	first := flag.String("first", config.DefaultModel, "model for the first prompt")
	then := flag.String("then", "anthropic/claude-haiku-4-5-20251001", "model for the second prompt")
	flag.Parse()

	firstRef, err := message.ParseModelRef(*first)
	if err != nil {
		log.Fatal(err)
	}
	thenRef, err := message.ParseModelRef(*then)
	if err != nil {
		log.Fatal(err)
	}

	s := engine.NewSession(engine.Config{
		Providers: registry(),
		Model:     firstRef,
		OnEvent: func(e engine.Event) {
			if e.Type == engine.EventTextDelta {
				fmt.Print(e.Text)
			}
		},
	})
	ctx := context.Background()

	fmt.Printf("[%s]\n", s.Model())
	if _, err := s.Prompt(ctx, "Pick a random animal. Reply with only its name."); err != nil {
		log.Fatal(err)
	}

	s.SetModel(thenRef)
	fmt.Printf("\n\n[%s]\n", s.Model())
	if _, err := s.Prompt(ctx, "Which animal did you pick? Give one fact about it."); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
}

// registry adds a provider for each API key in the environment.
func registry() provider.Registry {
	reg := provider.Registry{}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		reg[anthropic.Family] = &anthropic.Client{APIKey: key}
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		reg[openai.Family] = &openai.Client{APIKey: key}
	}
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		reg["openrouter"] = &openaicompat.Client{
			Family:  "openrouter",
			APIKey:  key,
			BaseURL: "https://openrouter.ai/api/v1",
		}
	}
	if len(reg) == 0 {
		log.Fatal("set ANTHROPIC_API_KEY, OPENAI_API_KEY, or OPENROUTER_API_KEY")
	}
	return reg
}
