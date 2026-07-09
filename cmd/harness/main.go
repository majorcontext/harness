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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
	"github.com/majorcontext/harness/provider/openai"
	"github.com/majorcontext/harness/provider/openaicompat"
	"github.com/majorcontext/harness/server"
)

// defaultOpenRouterName is the providers map key that gets a built-in
// registration when config supplies none: the two-line openai-compat config
// case becomes a zero-line case for OpenRouter specifically. Any config
// entry named "openrouter" — of any Type — overrides this default entirely.
const defaultOpenRouterName = "openrouter"

// defaultOpenRouterBaseURL and defaultOpenRouterAPIKeyEnv are OpenRouter's
// well-known chat-completions endpoint and the env var convention for its
// key; see https://openrouter.ai/docs.
const (
	defaultOpenRouterBaseURL   = "https://openrouter.ai/api/v1"
	defaultOpenRouterAPIKeyEnv = "OPENROUTER_API_KEY"
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
			// A goal that ran to completion but was not achieved exits 3; its
			// final status is already on stderr, so don't print again.
			if errors.Is(err, errGoalNotAchieved) {
				os.Exit(3)
			}
			fmt.Fprintln(os.Stderr, "harness:", err)
			os.Exit(1)
		}
	case "sessions":
		if err := sessionsCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "harness:", err)
			os.Exit(1)
		}
	case "serve":
		if err := serveCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "harness:", err)
			os.Exit(1)
		}
	case "plugin":
		if err := pluginCmd(os.Args[2:]); err != nil {
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
  harness run -goal <condition> [flags]
                                    pursue a goal until an evaluator judges it
                                    met (exit 0 achieved, 3 not achieved)
  harness serve [-addr host:port] [-cors-origin origin] [-no-instructions]
                [-skills-dir dir ...]
                                    serve the HTTP+SSE session API
  harness plugin probe              re-probe configured plugins and refresh
                                    the manifest cache
  harness sessions [--json]         list persisted sessions
  harness version                   print version

run flags:
`)
	runFlags(nil).PrintDefaults()
}

type runOptions struct {
	prompt         string
	goal           string
	goalMaxTurns   int
	model          string
	system         string
	maxTokens      int
	jsonOut        bool
	noSave         bool
	noInstructions bool
	skillsDirs     []string
	resume         string
	cont           bool
}

func runFlags(opts *runOptions) *flag.FlagSet {
	if opts == nil {
		opts = &runOptions{}
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.prompt, "p", "", "the prompt (required unless -goal is given)")
	fs.StringVar(&opts.goal, "goal", "", "pursue a goal: prompt this condition, then re-prompt with evaluator feedback until an independent evaluator judges it met (requires config goal_evaluator_model)")
	fs.IntVar(&opts.goalMaxTurns, "goal-max-turns", 0, "maximum turns for -goal (0 = unlimited)")
	fs.StringVar(&opts.model, "model", "", "model ref (provider/model) or alias; overrides the persisted model when resuming; default from config, else "+config.DefaultModel)
	fs.StringVar(&opts.system, "system", "", "extra system prompt segment")
	fs.IntVar(&opts.maxTokens, "max-tokens", 0, "per-response output token cap")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit the event stream as JSON lines instead of text")
	fs.BoolVar(&opts.noSave, "no-save", false, "disable session persistence")
	fs.BoolVar(&opts.noInstructions, "no-instructions", false, "do not inject the project's AGENTS.md into the system prompt")
	fs.Func("skills-dir", "directory of Agent Skills to advertise (repeatable); overrides config skills_dirs; default <workdir>/.agents/skills when present", func(v string) error {
		opts.skillsDirs = append(opts.skillsDirs, v)
		return nil
	})
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

// sessionJSON is the wire shape for `harness sessions --json`: one object
// per session with created_at marshaled via time.Time's default JSON
// encoding (RFC3339 with nanoseconds), matching the server's session wire
// shape and mirroring engine.SessionInfo.
type sessionJSON struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Messages  int       `json:"messages"`
}

// formatSessionsJSON renders the session list as a JSON array. An empty
// list yields "[]" rather than "null" so consumers always get an array.
func formatSessionsJSON(infos []engine.SessionInfo) (string, error) {
	out := make([]sessionJSON, 0, len(infos))
	for _, info := range infos {
		out = append(out, sessionJSON{
			ID:        info.ID,
			CreatedAt: info.CreatedAt,
			Messages:  info.Messages,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func sessionsCmd(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "emit the session list as a JSON array")
	if err := fs.Parse(args); err != nil {
		return err
	}

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
	if jsonOut {
		out, err := formatSessionsJSON(infos)
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
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
	switch {
	case opts.prompt == "" && opts.goal == "":
		return fmt.Errorf("-p <prompt> or -goal <condition> is required")
	case opts.prompt != "" && opts.goal != "":
		return fmt.Errorf("-p and -goal are mutually exclusive")
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

	// mcpMgr's defer is declared before the plugin host's below, so (defers
	// unwind LIFO) it closes MCP server connections only after the plugin
	// host has closed — a plugin's client/mcp.call has nowhere left to route
	// once the host is gone anyway, so shutting the host down first is the
	// safer order.
	mcpMgr := buildMCPManager(cfg.MCPServers)
	defer closeMCPManager(mcpMgr)

	lateAPI := newLateClientAPI()
	host, err := buildPluginHost(ctx, cfg.Plugins, version, workDir, cfg.PluginHTTPHeaders, lateAPI, "", "")
	if err != nil {
		return err
	}
	// Deferred here so it runs after this whole command (including the
	// Prompt/PursueGoal call below) completes — plugins stay warm for the
	// run, exactly like a served session.
	defer func() {
		if host != nil {
			host.Close()
		}
	}()

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

	// The plugin host's ClientAPI is the direct engine-backed adapter (see
	// cmd/harness/clientapi.go), late-bound: sess is assigned immediately
	// below once resolveSession returns, strictly before the first
	// Prompt/PursueGoal call — the earliest point any hook can fire.
	var sess *engine.Session
	lateAPI.Bind(newLazyRunClientAPI(func() *engine.Session { return sess }))

	s, err := resolveSession(engine.Config{
		Providers:    registry(cfg),
		Model:        model,
		System:       systemPrompt(workDir, opts.system),
		MaxTokens:    opts.maxTokens,
		WorkDir:      workDir,
		SessionDir:   sesDir,
		OnEvent:      onEvent,
		Instructions: instructionsConfig(cfg, opts.noInstructions),
		SkillsDirs:   skillsDirs(cfg, opts.skillsDirs, workDir),
		Hooks:        pluginHooks(host),
		MCP:          mcpRegistry(mcpMgr),
	}, opts.resume, opts.cont, modelSet)
	if err != nil {
		return err
	}
	sess = s

	goalNotAchieved := false
	if opts.goal != "" {
		res, err := runGoal(ctx, cfg, s, opts)
		if err != nil {
			return err
		}
		goalNotAchieved = !res.Achieved
	} else {
		if _, err := s.Prompt(ctx, opts.prompt); err != nil {
			return err
		}
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
	if goalNotAchieved {
		return errGoalNotAchieved
	}
	return nil
}

// errGoalNotAchieved is a sentinel: `harness run -goal` returns it when the
// evaluator never judged the condition met. main maps it to exit code 3 (the
// final status has already been printed to stderr), distinct from exit 1 for a
// genuine failure.
var errGoalNotAchieved = errors.New("goal not achieved")

// runGoal resolves the configured evaluator model and drives PursueGoal to
// completion, printing the final status to stderr.
func runGoal(ctx context.Context, cfg *config.Config, s *engine.Session, opts runOptions) (*engine.GoalResult, error) {
	if cfg.GoalEvaluatorModel == "" {
		return nil, fmt.Errorf("goal_evaluator_model must be set in config to use -goal")
	}
	evaluator, err := cfg.ResolveModel(cfg.GoalEvaluatorModel)
	if err != nil {
		return nil, fmt.Errorf("goal_evaluator_model: %w", err)
	}
	res, err := s.PursueGoal(ctx, opts.goal, engine.GoalOptions{
		MaxTurns:  opts.goalMaxTurns,
		Evaluator: evaluator,
	})
	if err != nil {
		return nil, err
	}
	if res.Achieved {
		fmt.Fprintf(os.Stderr, "goal achieved in %d turn(s): %s\n", res.Turns, res.Reason)
	} else {
		fmt.Fprintf(os.Stderr, "goal not achieved after %d turn(s): %s\n", res.Turns, res.Reason)
	}
	return res, nil
}

// loadConfig loads the effective configuration once: the user config file
// plus, if present, the current directory's project override. This is the only
// disk access on the boot path (at most two file reads; missing files are
// fine) — no network, no process spawn, no directory creation.
func loadConfig() (*config.Config, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return config.LoadProject(dir)
}

// registry wires up all known provider adapters. Keys are ModelRef.Provider
// values. Auth is read here but validated only on first send. Adding
// another built-in provider family is a two-line change: resolve its config
// with providerAuth and add one entry to the returned map. Any config
// providers entry with type "openai-compat" needs no code at all — see
// registerOpenAICompatProviders — and OpenRouter itself needs no config
// entry either, see ensureDefaultOpenRouter.
//
// registry does not assume cfg came from config.LoadProject (the load path
// that guarantees nativeDefaultProviders fields are filled in — see
// config.EnsureProviderDefaults): it calls EnsureProviderDefaults itself
// first, idempotently, so a hand-built *config.Config (as tests use, and
// any future embedder that skips LoadProject might too) resolves a minimal
// {"openrouter": {"api_key_env": "..."}} entry identically to one that went
// through the full config-loading choke point, rather than silently
// registering no adapter for it at all.
func registry(cfg *config.Config) provider.Registry {
	if cfg != nil {
		config.EnsureProviderDefaults(cfg.Providers)
	}
	akey, abase := providerAuth(cfg, anthropic.Family, "ANTHROPIC_API_KEY")
	okey, obase := providerAuth(cfg, openai.Family, "OPENAI_API_KEY")
	reg := provider.Registry{
		anthropic.Family: &anthropic.Client{APIKey: akey, BaseURL: abase},
		openai.Family:    &openai.Client{APIKey: okey, BaseURL: obase},
	}
	registerOpenAICompatProviders(reg, cfg)
	ensureDefaultOpenRouter(reg, cfg)
	return reg
}

// registerOpenAICompatProviders builds a provider/openaicompat client for
// every config.Providers entry of config.TypeOpenAICompat, keyed by its
// providers map name — that name is what routes "name/model" refs to it,
// exactly like a built-in family. config.Load already rejects unknown
// Type values, so nothing here needs to guard against typos.
func registerOpenAICompatProviders(reg provider.Registry, cfg *config.Config) {
	if cfg == nil {
		return
	}
	for name, p := range cfg.Providers {
		if p.Type != config.TypeOpenAICompat {
			continue
		}
		reg[name] = newOpenAICompatClient(name, p)
	}
}

// ensureDefaultOpenRouter registers the "openrouter" family with
// OpenRouter's well-known base URL and API key env var when config supplies
// no "openrouter" entry at all (of any Type) — making the common case zero
// lines of config. An explicit config entry, including one that overrides
// only some fields, replaces this default entirely (registerOpenAICompatProviders
// above already wrote it into reg by the time this runs).
func ensureDefaultOpenRouter(reg provider.Registry, cfg *config.Config) {
	if cfg != nil {
		if _, ok := cfg.Providers[defaultOpenRouterName]; ok {
			return
		}
	}
	reg[defaultOpenRouterName] = &openaicompat.Client{
		Family:  defaultOpenRouterName,
		APIKey:  os.Getenv(defaultOpenRouterAPIKeyEnv),
		BaseURL: defaultOpenRouterBaseURL,
	}
}

// newOpenAICompatClient builds one openaicompat.Client from a config
// entry. Family defaults to the providers map key (name) when the entry
// does not override it; APIKeyEnv empty means no key env configured, which
// leaves APIKey empty (the adapter reports that loudly on first Stream, not
// here — auth is validated on first send, per the startup speed rule).
func newOpenAICompatClient(name string, p config.Provider) *openaicompat.Client {
	family := p.Family
	if family == "" {
		family = name
	}
	var apiKey string
	if p.APIKeyEnv != "" {
		apiKey = os.Getenv(p.APIKeyEnv)
	}
	return &openaicompat.Client{
		Family:       family,
		APIKey:       apiKey,
		BaseURL:      p.BaseURL,
		ExtraHeaders: p.ExtraHeaders,
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

// serveURLForAddr derives the URL plugins should use to reach this
// process's `harness serve` HTTP API from the -addr flag's listen address.
// A bind-all address isn't reliably dialable as-is from another process on
// the same host, so an empty host or an unspecified IP (0.0.0.0 or ::,
// meaning "listen on every interface") is rewritten to the loopback address
// 127.0.0.1; any other, explicit host (e.g. localhost, 10.0.0.5) is kept
// as-is.
func serveURLForAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not a valid host:port (shouldn't happen for a listen address); fall
		// back to the previous verbatim behavior.
		return "http://" + addr
	}
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// serveCmd starts the HTTP+SSE session API. The run token comes from
// HARNESS_RUN_TOKEN (required); the listener opens at boot, but nothing here
// touches network egress, spawns processes, or scans beyond the session dir —
// provider auth still validates on first message send.
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var addr string
	fs.StringVar(&addr, "addr", "localhost:4096", "listen address")
	var corsOrigin string
	fs.StringVar(&corsOrigin, "cors-origin", "", "enable browser CORS by echoing this Access-Control-Allow-Origin value (e.g. your inspector origin, or * for dev); empty disables CORS")
	var noInstructions bool
	fs.BoolVar(&noInstructions, "no-instructions", false, "disable automatic AGENTS.md injection for sessions served by this instance")
	var skillDirs []string
	fs.Func("skills-dir", "directory of Agent Skills to advertise (repeatable); overrides config skills_dirs", func(v string) error {
		skillDirs = append(skillDirs, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	token := os.Getenv("HARNESS_RUN_TOKEN")
	if token == "" {
		return fmt.Errorf("HARNESS_RUN_TOKEN is required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	defModel, err := cfg.ResolveModel("")
	if err != nil {
		return err
	}
	// Resolve the goal evaluator model up front (empty leaves it zero, so goal
	// requests are rejected until goal_evaluator_model is configured).
	var goalEval message.ModelRef
	if cfg.GoalEvaluatorModel != "" {
		goalEval, err = cfg.ResolveModel(cfg.GoalEvaluatorModel)
		if err != nil {
			return fmt.Errorf("goal_evaluator_model: %w", err)
		}
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	sesDir, err := sessionDir(false, cfg.SessionDir)
	if err != nil {
		return err
	}
	reg := registry(cfg)

	// Every session shares the same MCP client connections; built once here
	// and closed on exit. Its defer is declared before the plugin host's
	// below, so (defers unwind LIFO) it closes after the host — see the
	// matching comment in runCmd.
	mcpMgr := buildMCPManager(cfg.MCPServers)
	defer closeMCPManager(mcpMgr)

	// Every session gets the same plugin host; it is built once here and
	// closed on exit (deferred before srv's own defer below, so — since
	// defers unwind LIFO — the host outlives the server's shutdown/drain and
	// closes only after it, matching a served session's own lifetime).
	lateAPI := newLateClientAPI()
	pluginHost, err := buildPluginHost(context.Background(), cfg.Plugins, version, workDir, cfg.PluginHTTPHeaders, lateAPI, serveURLForAddr(addr), token)
	if err != nil {
		return err
	}
	defer func() {
		if pluginHost != nil {
			pluginHost.Close()
		}
	}()

	// Structured logging: JSON to stderr, stdlib log/slog only (no new
	// dependency). serve start and every OnError go through it; this is
	// intentionally minimal — no request-level access logging, no metrics, no
	// OTel (a separate future cmd-scoped task).
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// The event journal owner needs each engine session to report events to
	// it, so the session wrappers wire OnEvent to the server's Publish.
	// host is built just below, once srv exists (its ClientAPI is
	// server-backed — see server/clientapi.go); mkCfg closes over the srv
	// variable, so it can reference it before it is assigned — same pattern
	// as the OnEvent closure above it.
	var srv *server.Server
	mkCfg := func(model message.ModelRef) engine.Config {
		return engine.Config{
			Providers:    reg,
			Model:        model,
			System:       systemPrompt(workDir, ""),
			WorkDir:      workDir,
			SessionDir:   sesDir,
			OnEvent:      func(ev engine.Event) { srv.Publish(ev) },
			Instructions: instructionsConfig(cfg, noInstructions),
			SkillsDirs:   skillsDirs(cfg, skillDirs, workDir),
			Hooks:        pluginHooks(pluginHost),
			MCP:          mcpRegistry(mcpMgr),
		}
	}
	srv, err = server.New(server.Options{
		SessionDir:    sesDir,
		RunToken:      token,
		Version:       version,
		CORSOrigin:    corsOrigin,
		GoalEvaluator: goalEval,
		MCP:           mcpRegistry(mcpMgr),
		OnError: func(_ context.Context, err error) {
			logger.Error("serve error", "error", err.Error())
		},
		NewSession:  newSessionFn(mkCfg, defModel, cfg, skillDirs, func(id string, turn int, req *provider.Request) { srv.OnRequest(id, turn, req) }),
		LoadSession: loadSessionFn(mkCfg, defModel, cfg, skillDirs, func(id string, turn int, req *provider.Request) { srv.OnRequest(id, turn, req) }),
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	lateAPI.Bind(srv.ClientAPI())

	httpSrv := &http.Server{Addr: addr, Handler: srv}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	logger.Info("serve start", "addr", addr, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Run Shutdown and Drain CONCURRENTLY under one deadline (see
		// server.Shutdown). Shutdown closes the listener immediately, so no new
		// request is admitted the instant we begin; Drain closes the SSE tails so
		// Shutdown returns promptly, while in parallel the detached prompt
		// goroutines get the full grace budget before they are cancelled. Draining
		// before the deferred srv.Close keeps the journal file open until the
		// trailing assistant message and session.aborted/idle records land.
		return server.Shutdown(shutCtx, httpSrv, srv)
	}
}

// newSessionFn builds server.Options.NewSession. mkCfg's base cfg.System and
// cfg.SkillsDirs name the process cwd (mkCfg is shared with loadSessionFn and
// the non-serve run path, where that is correct); a served session with an
// explicit workdir gets its own working directory instead, so both must be
// rebuilt from sessionWorkDir — otherwise a session rooted elsewhere would
// carry a "Working directory:" line naming the wrong directory, and a
// relative skills_dirs entry (appCfg/flagDirs) would be resolved against the
// wrong directory too, discovering the wrong skills (or none). onRequest is
// wired to the server's request journal, keyed by the session's own ID
// (assigned by engine.NewSession, so it cannot be captured until after
// construction).
func newSessionFn(mkCfg func(message.ModelRef) engine.Config, defModel message.ModelRef, appCfg *config.Config, flagDirs []string, onRequest func(id string, turn int, req *provider.Request)) func(message.ModelRef, string) (*engine.Session, error) {
	return func(model message.ModelRef, sessionWorkDir string) (*engine.Session, error) {
		if model.IsZero() {
			model = defModel
		}
		cfg := mkCfg(model)
		// The server has already resolved and validated sessionWorkDir
		// (defaulting to this process's cwd when the caller omitted one; see
		// server.Options.WorkspaceRoots) — it wins over the process cwd for
		// this session's tools, AGENTS.md discovery, Agent Skills default
		// directory, and (below) the system prompt's working-directory line.
		cfg.WorkDir = sessionWorkDir
		cfg.System = systemPrompt(sessionWorkDir, "")
		cfg.SkillsDirs = skillsDirs(appCfg, flagDirs, sessionWorkDir)
		var sess *engine.Session
		cfg.OnRequest = func(turn int, req *provider.Request) { onRequest(sess.ID, turn, req) }
		sess = engine.NewSession(cfg)
		return sess, nil
	}
}

// loadSessionFn builds server.Options.LoadSession. engine.LoadSession
// restores the session's durable WorkDir from its log header, which wins
// over the cfg.WorkDir passed in (see engine/store.go) — but the cfg.System
// and cfg.SkillsDirs built by mkCfg still name the process cwd. When the
// restored directory differs, this rebuilds both from it and reloads, so a
// resumed session's system prompt names its own working directory and a
// relative skills_dirs entry (appCfg/flagDirs) resolves against it rather
// than whichever directory this process happened to start in. The reload is
// cheap (a second read of the same on-disk log) and side-effect-free, since
// LoadSession is a pure rebuild from the journal.
func loadSessionFn(mkCfg func(message.ModelRef) engine.Config, defModel message.ModelRef, appCfg *config.Config, flagDirs []string, onRequest func(id string, turn int, req *provider.Request)) func(string) (*engine.Session, error) {
	return func(id string) (*engine.Session, error) {
		cfg := mkCfg(defModel)
		wire := func(c engine.Config) (*engine.Session, error) {
			var sess *engine.Session
			c.OnRequest = func(turn int, req *provider.Request) { onRequest(sess.ID, turn, req) }
			sess, err := engine.LoadSession(c, id)
			return sess, err
		}
		sess, err := wire(cfg)
		if err != nil {
			return nil, err
		}
		if wd := sess.WorkDir(); wd != cfg.WorkDir {
			cfg.WorkDir = wd
			cfg.System = systemPrompt(wd, "")
			cfg.SkillsDirs = skillsDirs(appCfg, flagDirs, wd)
			sess, err = wire(cfg)
		}
		return sess, err
	}
}

// instructionsConfig translates the -no-instructions flag and config file
// fields into the engine's InstructionsConfig. Precedence: the flag disables
// unconditionally; otherwise config `instructions: false` disables, config
// `instructions_path` names an override, and anything else returns nil (the
// engine default: auto-discover AGENTS.md by walking up from WorkDir).
func instructionsConfig(cfg *config.Config, noInstructions bool) *engine.InstructionsConfig {
	if noInstructions {
		return &engine.InstructionsConfig{Disabled: true}
	}
	if cfg == nil {
		return nil
	}
	if cfg.Instructions != nil && !*cfg.Instructions {
		return &engine.InstructionsConfig{Disabled: true}
	}
	if cfg.InstructionsPath != "" {
		return &engine.InstructionsConfig{Path: cfg.InstructionsPath}
	}
	return nil
}

// skillsDirs resolves the effective Agent Skills directories for the engine.
// Precedence: repeatable -skills-dir flags override config skills_dirs
// entirely; otherwise config skills_dirs is used. Relative entries resolve
// against workDir. When neither is set it returns nil, leaving the engine
// default in place (use <workDir>/.agents/skills when it exists).
func skillsDirs(cfg *config.Config, flagDirs []string, workDir string) []string {
	dirs := flagDirs
	if len(dirs) == 0 && cfg != nil && cfg.SkillsDirs != nil {
		// A config file's explicit "skills_dirs": [] is an opt-out and must
		// stay a non-nil empty slice; only a truly absent field falls
		// through to nil (engine default discovery).
		dirs = cfg.SkillsDirs
	}
	if dirs == nil {
		return nil
	}
	if len(dirs) == 0 {
		return []string{}
	}
	out := make([]string, len(dirs))
	for i, d := range dirs {
		if filepath.IsAbs(d) {
			out[i] = d
		} else {
			out[i] = filepath.Join(workDir, d)
		}
	}
	return out
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
