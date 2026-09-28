// Prompt sends one prompt to a model and streams the reply to stdout. The
// model can use the built-in tools (bash, file reads and edits) in the current
// directory.
//
//	ANTHROPIC_API_KEY=... go run ./examples/prompt "Summarize README.md"
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

func main() {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		log.Fatal("set ANTHROPIC_API_KEY")
	}
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in this directory."
	}
	model, err := message.ParseModelRef(config.DefaultModel)
	if err != nil {
		log.Fatal(err)
	}

	s := engine.NewSession(engine.Config{
		Providers: provider.Registry{anthropic.Family: &anthropic.Client{APIKey: key}},
		Model:     model,
		WorkDir:   ".",
		OnEvent: func(e engine.Event) {
			if e.Type == engine.EventTextDelta {
				fmt.Print(e.Text)
			}
		},
	})
	if _, err := s.Prompt(context.Background(), prompt); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
}
