package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

// A session with one provider. The reply streams through OnEvent.
func ExampleNewSession() {
	s := engine.NewSession(engine.Config{
		Providers: provider.Registry{
			anthropic.Family: &anthropic.Client{APIKey: os.Getenv("ANTHROPIC_API_KEY")},
		},
		Model:   message.ModelRef{Provider: anthropic.Family, Model: "claude-fable-5"},
		WorkDir: ".",
		OnEvent: func(e engine.Event) {
			if e.Type == engine.EventTextDelta {
				fmt.Print(e.Text)
			}
		},
	})
	if _, err := s.Prompt(context.Background(), "List the files in this directory."); err != nil {
		log.Fatal(err)
	}
}

// A Go function that the model can call.
func ExampleTool() {
	now := engine.Tool{
		Def: provider.ToolDef{
			Name:        "current_time",
			Description: "Return the current time in RFC 3339 format.",
			InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		},
		Run: func(ctx context.Context, s *engine.Session, args json.RawMessage) (message.Parts, error) {
			return message.Parts{&message.Text{Text: time.Now().Format(time.RFC3339)}}, nil
		},
	}

	s := engine.NewSession(engine.Config{
		Providers: provider.Registry{
			anthropic.Family: &anthropic.Client{APIKey: os.Getenv("ANTHROPIC_API_KEY")},
		},
		Model: message.ModelRef{Provider: anthropic.Family, Model: "claude-fable-5"},
		Tools: []engine.Tool{now},
	})
	reply, err := s.Prompt(context.Background(), "What time is it?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(reply.Parts.Text())
}

// Change the model between prompts. The next model receives the full history.
func ExampleSession_SetModel() {
	s := engine.NewSession(engine.Config{
		Providers: provider.Registry{
			anthropic.Family: &anthropic.Client{APIKey: os.Getenv("ANTHROPIC_API_KEY")},
		},
		Model: message.ModelRef{Provider: anthropic.Family, Model: "claude-fable-5"},
	})
	ctx := context.Background()
	if _, err := s.Prompt(ctx, "Pick a random animal."); err != nil {
		log.Fatal(err)
	}

	s.SetModel(message.ModelRef{Provider: anthropic.Family, Model: "claude-haiku-4-5-20251001"})
	reply, err := s.Prompt(ctx, "Which animal did you pick?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(reply.Parts.Text())
}
