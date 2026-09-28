// Custom-tool gives the model a Go function to call. The model calls
// roll_dice, and the reply uses the result.
//
//	ANTHROPIC_API_KEY=... go run ./examples/custom-tool
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strconv"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

var rollDice = engine.Tool{
	Def: provider.ToolDef{
		Name:        "roll_dice",
		Description: "Roll a die with the given number of sides and return the result.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"sides": {"type": "integer", "minimum": 2}},
			"required": ["sides"]
		}`),
	},
	Run: func(ctx context.Context, s *engine.Session, args json.RawMessage) (message.Parts, error) {
		var in struct {
			Sides int `json:"sides"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		if in.Sides < 2 {
			return nil, fmt.Errorf("sides must be at least 2, got %d", in.Sides)
		}
		roll := rand.IntN(in.Sides) + 1
		return message.Parts{&message.Text{Text: strconv.Itoa(roll)}}, nil
	},
}

func main() {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		log.Fatal("set ANTHROPIC_API_KEY")
	}
	model, err := message.ParseModelRef(config.DefaultModel)
	if err != nil {
		log.Fatal(err)
	}

	s := engine.NewSession(engine.Config{
		Providers: provider.Registry{anthropic.Family: &anthropic.Client{APIKey: key}},
		Model:     model,
		Tools:     []engine.Tool{rollDice},
		OnEvent: func(e engine.Event) {
			switch e.Type {
			case engine.EventTextDelta:
				fmt.Print(e.Text)
			case engine.EventToolStart:
				fmt.Printf("\n[%s %s]\n", e.ToolCall.Name, e.ToolCall.Arguments)
			}
		},
	})
	if _, err := s.Prompt(context.Background(), "Roll a 20-sided die twice and tell me the total."); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
}
