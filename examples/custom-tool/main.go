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

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// rollDice is a harness.Tool: a spec for the model and a function to run.
type rollDice struct{}

func (rollDice) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name:        "roll_dice",
		Description: "Roll a die with the given number of sides and return the result.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"sides": {"type": "integer", "minimum": 2}},
			"required": ["sides"]
		}`),
	}
}

func (rollDice) Run(_ context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in struct {
		Sides int `json:"sides"`
	}
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, err
	}
	if in.Sides < 2 {
		return protocol.ToolResult{}, fmt.Errorf("sides must be at least 2, got %d", in.Sides)
	}
	return protocol.ToolResult{Text: strconv.Itoa(rand.IntN(in.Sides) + 1)}, nil
}

func main() {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Fatal("set ANTHROPIC_API_KEY")
	}
	ctx := context.Background()

	rt, err := harness.New(harness.Options{Store: harness.NewMemStore(), Tools: []harness.Tool{rollDice{}}})
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)
	s, err := rt.Create(ctx, protocol.CreateSession{})
	if err != nil {
		log.Fatal(err)
	}
	head := s.View().HeadSeq
	prompt := "Roll a 20-sided die twice and tell me the total."
	if _, err := s.Submit(ctx, protocol.Input{ID: "prompt-1", Parts: []protocol.Part{{Type: protocol.PartText, Text: prompt}}}); err != nil {
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
		case "item.completed":
			var it struct {
				Message struct {
					Parts []struct {
						Type, Name string
						Arguments  json.RawMessage
					} `json:"parts"`
				} `json:"message"`
			}
			if json.Unmarshal(e.Data, &it) != nil {
				continue
			}
			for _, p := range it.Message.Parts {
				if p.Type == "tool_call" {
					fmt.Printf("\n[%s %s]\n", p.Name, p.Arguments)
				}
			}
		case "turn.ended":
			fmt.Println()
			return
		}
	}
}
