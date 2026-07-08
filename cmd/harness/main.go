// Command harness is the CLI for the harness agent engine.
//
// Startup speed is a budget (see AGENTS.md): nothing here touches the
// network, spawns processes, or reads more than flags before first output.
// Provider auth is validated on first message send, not at boot.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/andybons/harness/engine"
	"github.com/andybons/harness/message"
	"github.com/andybons/harness/provider"
	"github.com/andybons/harness/provider/anthropic"
)

var version = "0.1.0-dev"

const defaultModel = "anthropic/claude-fable-5"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("harness " + version)
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "harness:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  harness run -p <prompt> [flags]   run a one-shot prompt
  harness version                   print version

run flags:
`)
	runFlags(nil).PrintDefaults()
}

type runOptions struct {
	prompt    string
	model     string
	system    string
	maxTokens int
	jsonOut   bool
}

func runFlags(opts *runOptions) *flag.FlagSet {
	if opts == nil {
		opts = &runOptions{}
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.prompt, "p", "", "the prompt (required)")
	fs.StringVar(&opts.model, "model", defaultModel, "model ref, provider/model")
	fs.StringVar(&opts.system, "system", "", "extra system prompt segment")
	fs.IntVar(&opts.maxTokens, "max-tokens", 0, "per-response output token cap")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit the event stream as JSON lines instead of text")
	return fs
}

func runCmd(args []string) error {
	var opts runOptions
	if err := runFlags(&opts).Parse(args); err != nil {
		return err
	}
	if opts.prompt == "" {
		return fmt.Errorf("-p <prompt> is required")
	}
	model, err := message.ParseModelRef(opts.model)
	if err != nil {
		return err
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	printedText := false
	onEvent := func(ev engine.Event) {
		if opts.jsonOut {
			enc.Encode(ev) //nolint:errcheck
			return
		}
		switch ev.Type {
		case engine.EventTextDelta:
			fmt.Print(ev.Text)
			printedText = true
		case engine.EventToolStart:
			fmt.Fprintf(os.Stderr, "\n[tool %s] %s\n", ev.ToolCall.Name, ev.ToolCall.Arguments)
		case engine.EventToolEnd:
			if ev.IsError {
				fmt.Fprintf(os.Stderr, "[tool %s failed] %s\n", ev.ToolCall.Name, ev.Output.Text())
			}
		}
	}

	s := engine.NewSession(engine.Config{
		Providers: registry(),
		Model:     model,
		System:    systemPrompt(workDir, opts.system),
		MaxTokens: opts.maxTokens,
		WorkDir:   workDir,
		OnEvent:   onEvent,
	})

	if _, err := s.Prompt(ctx, opts.prompt); err != nil {
		return err
	}
	if printedText {
		fmt.Println()
	}
	return nil
}

// registry wires up all known provider adapters. Keys are ModelRef.Provider
// values. Auth is read here but validated only on first send.
func registry() provider.Registry {
	return provider.Registry{
		anthropic.Family: &anthropic.Client{APIKey: os.Getenv("ANTHROPIC_API_KEY")},
	}
}

func systemPrompt(workDir, extra string) []string {
	system := []string{
		"You are harness, a fast coding agent. You execute tasks directly " +
			"using the tools available to you and report results concisely.\n\n" +
			"Working directory: " + workDir,
	}
	if extra != "" {
		system = append(system, extra)
	}
	return system
}
