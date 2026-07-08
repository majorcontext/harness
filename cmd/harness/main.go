// Command harness is the CLI for the harness agent engine.
//
// Startup speed is a budget (see AGENTS.md): nothing here touches the
// network, spawns processes, or reads more than flags before first output.
// Provider auth is validated on first message send, not at boot. Session
// persistence is lazy too: the engine creates the session directory and log
// file on first message append, and the CLI reads the directory only when
// -c/-r/sessions ask for it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

var version = "0.1.0-dev"

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
	case "sessions":
		if err := sessionsCmd(); err != nil {
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
  harness sessions                  list persisted sessions
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
	noSave    bool
	resume    string
	cont      bool
}

func runFlags(opts *runOptions) *flag.FlagSet {
	if opts == nil {
		opts = &runOptions{}
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.prompt, "p", "", "the prompt (required)")
	fs.StringVar(&opts.model, "model", "", "model ref (provider/model) or alias; overrides the persisted model when resuming; default from config, else "+config.DefaultModel)
	fs.StringVar(&opts.system, "system", "", "extra system prompt segment")
	fs.IntVar(&opts.maxTokens, "max-tokens", 0, "per-response output token cap")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit the event stream as JSON lines instead of text")
	fs.BoolVar(&opts.noSave, "no-save", false, "disable session persistence")
	fs.StringVar(&opts.resume, "r", "", "resume the session with this id")
	fs.StringVar(&opts.resume, "resume", "", "resume the session with this id")
	fs.BoolVar(&opts.cont, "c", false, "continue the most recent session")
	fs.BoolVar(&opts.cont, "continue", false, "continue the most recent session")
	return fs
}

// sessionDir resolves where session logs live, in precedence order:
// -no-save (yields "", persistence disabled) > $HARNESS_SESSION_DIR >
// configDir (config session_dir) > $HOME/.harness/sessions. Nothing is
// created here; the engine creates the directory lazily on first write.
func sessionDir(noSave bool, configDir string) (string, error) {
	if noSave {
		return "", nil
	}
	if dir := os.Getenv("HARNESS_SESSION_DIR"); dir != "" {
		return dir, nil
	}
	if configDir != "" {
		return configDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".harness", "sessions"), nil
}

// resolveSession creates or resumes the session for a run: a fresh session
// by default, the named one for -r, the most recently created one for -c.
//
// modelSet reports whether -model was passed explicitly. Explicit flags
// always win: on resume, cfg.Model then overrides the session's persisted
// model via SetModel (which also persists a model record). Without an
// explicit -model the persisted model is retained.
func resolveSession(cfg engine.Config, resume string, cont bool, modelSet bool) (*engine.Session, error) {
	switch {
	case resume != "" && cont:
		return nil, fmt.Errorf("-r and -c are mutually exclusive")
	case (resume != "" || cont) && cfg.SessionDir == "":
		return nil, fmt.Errorf("cannot resume a session with -no-save")
	}

	var id string
	switch {
	case resume != "":
		id = resume
	case cont:
		infos, err := engine.ListSessions(cfg.SessionDir)
		if err != nil {
			return nil, err
		}
		if len(infos) == 0 {
			return nil, fmt.Errorf("no sessions to continue")
		}
		id = infos[len(infos)-1].ID
	default:
		return engine.NewSession(cfg), nil
	}

	s, err := engine.LoadSession(cfg, id)
	if err != nil {
		return nil, err
	}
	if modelSet {
		s.SetModel(cfg.Model)
	}
	return s, nil
}

// formatSessions renders one session per line: id, created_at (RFC3339),
// message count, tab-separated.
func formatSessions(infos []engine.SessionInfo) string {
	var b strings.Builder
	for _, info := range infos {
		fmt.Fprintf(&b, "%s\t%s\t%d\n", info.ID, info.CreatedAt.Format(time.RFC3339), info.Messages)
	}
	return b.String()
}

func sessionsCmd() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dir, err := sessionDir(false, cfg.SessionDir)
	if err != nil {
		return err
	}
	infos, err := engine.ListSessions(dir)
	if err != nil {
		return err
	}
	fmt.Print(formatSessions(infos))
	return nil
}

func runCmd(args []string) error {
	var opts runOptions
	fs := runFlags(&opts)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Visit walks only flags that were actually set, so modelSet is true
	// exactly when -model was passed explicitly — the signal resolveSession
	// uses to let the flag override a resumed session's persisted model.
	var modelSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "model" {
			modelSet = true
		}
	})
	if opts.prompt == "" {
		return fmt.Errorf("-p <prompt> is required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Aliases resolve here; an empty -model falls back to the config's
	// model, then the hard default.
	model, err := cfg.ResolveModel(opts.model)
	if err != nil {
		return err
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	sesDir, err := sessionDir(opts.noSave, cfg.SessionDir)
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

	s, err := resolveSession(engine.Config{
		Providers:  registry(cfg),
		Model:      model,
		System:     systemPrompt(workDir, opts.system),
		MaxTokens:  opts.maxTokens,
		WorkDir:    workDir,
		SessionDir: sesDir,
		OnEvent:    onEvent,
	}, opts.resume, opts.cont, modelSet)
	if err != nil {
		return err
	}

	if _, err := s.Prompt(ctx, opts.prompt); err != nil {
		return err
	}
	if printedText {
		fmt.Println()
	}
	if sesDir != "" {
		if perr := s.PersistErr(); perr != nil {
			fmt.Fprintln(os.Stderr, "harness: warning: session not persisted:", perr)
		} else {
			fmt.Fprintln(os.Stderr, "session:", s.ID)
		}
	}
	return nil
}

// loadConfig loads the effective configuration once: the user config file
// plus, if present, the current directory's project override. This is the only
// disk access on the boot path (one read, one stat) — no network, no process
// spawn, no directory creation.
func loadConfig() (*config.Config, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return config.LoadProject(dir)
}

// registry wires up all known provider adapters. Keys are ModelRef.Provider
// values. Auth is read here but validated only on first send. Adding another
// provider family is a two-line change: resolve its config with providerAuth
// and add one entry to the returned map.
func registry(cfg *config.Config) provider.Registry {
	akey, base := providerAuth(cfg, anthropic.Family, "ANTHROPIC_API_KEY")
	return provider.Registry{
		anthropic.Family: &anthropic.Client{APIKey: akey, BaseURL: base},
	}
}

// providerAuth resolves the API key and base URL for a provider family from
// config, falling back to defaultKeyEnv when no api_key_env is configured.
func providerAuth(cfg *config.Config, family, defaultKeyEnv string) (apiKey, baseURL string) {
	keyEnv := defaultKeyEnv
	if cfg != nil {
		if p, ok := cfg.Providers[family]; ok {
			if p.APIKeyEnv != "" {
				keyEnv = p.APIKeyEnv
			}
			baseURL = p.BaseURL
		}
	}
	return os.Getenv(keyEnv), baseURL
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
