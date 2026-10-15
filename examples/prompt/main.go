// Prompt sends one prompt to a model and streams the reply to stdout. The
// model can use the built-in tools (bash, file reads and edits) in the current
// directory.
//
//	go run ./examples/prompt "Summarize README.md"
//
// The model is the default of the machine: the API default when
// ANTHROPIC_API_KEY is set, else the ChatGPT login of "harness login codex",
// else the claude program on PATH.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/examples/internal/login"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in this directory."
	}
	ctx := context.Background()

	rt, err := harness.New(harness.Options{Store: store.NewMemStore(), WorkDir: ".", Config: login.Config(), Getenv: os.Getenv})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close(ctx) }()
	s, err := rt.Create(ctx, protocol.CreateSession{})
	if err != nil {
		log.Fatal(err)
	}
	t, err := s.Prompt(ctx, protocol.Input{ID: "prompt-1", Parts: []protocol.Part{{Type: protocol.PartText, Text: prompt}}})
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
	fmt.Println()
	res, err := t.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if res.StopReason == protocol.StopFailed {
		log.Fatal(res.Error)
	}
}
