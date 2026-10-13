// Guardrail runs a prompt with the guardrail plugin in the host process. The
// plugin blocks a force push, rm -rf on a broad target, and a read_file call on a
// .env file. The model sees each block as an error result.
//
//	ANTHROPIC_API_KEY=... go run ./examples/guardrail "Run git push --force"
//
// The plugin in examples/guardrail/cmd/guardrail-plugin runs the same value as
// a plugin process.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/examples/guardrail/internal/guard"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func main() {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Fatal("set ANTHROPIC_API_KEY")
	}
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "Run git push --force origin main."
	}
	ctx := context.Background()

	rt, err := harness.New(harness.Options{
		Store:   store.NewMemStore(),
		WorkDir: ".",
		Getenv:  os.Getenv,
		Plugins: []*plugin.Plugin{guard.Plugin},
	})
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
}
