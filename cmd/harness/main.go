// Command harness is the CLI for the harness agent engine.
//
// Startup speed is a budget (see cmd/harness/AGENTS.md): nothing here touches
// the network or spawns processes before the selected command needs it.
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
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
	"github.com/majorcontext/harness/provider/claudecode"
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

// slowPhaseThreshold surfaces store/create phases slower than this in serve
// (and run) logs, so a stalled create on a slow volume is diagnosable from a
// single spawn's logs without a debugger attached.
const slowPhaseThreshold = 1 * time.Second

// slowStorePhaseLogger returns an engine.Config.OnStorePhase callback that
// warns on any durable-store phase (engine/store.go's ensureLog,
// engine/queue.go's EnqueuePromptDurable) exceeding slowPhaseThreshold. Both
// serveCmd and runCmd wire it, symmetrically, since the store paths it
// observes don't differ between them. No per-phase Info logging here —
// EnqueuePromptDurable runs once per queued message, so an always-on line
// would spam; only the slow case is worth a serve log entry.
func slowStorePhaseLogger(logger *slog.Logger) func(op, phase string, elapsed time.Duration) {
	return func(op, phase string, elapsed time.Duration) {
		if elapsed > slowPhaseThreshold {
			logger.Warn("slow store phase", "op", op, "phase", phase, "elapsed_ms", elapsed.Milliseconds())
		}
	}
}

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
  harness serve [-addr host:port] [-unauthenticated] [--ask-user-question]
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
	agentDefsDirs  []string
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
	fs.StringVar(&opts.system, "system", "", "extra system prompt segment; appended after any config append_system_prompt segments")
	fs.IntVar(&opts.maxTokens, "max-tokens", 0, "per-response output token cap")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit the event stream as JSON lines instead of text")
	fs.BoolVar(&opts.noSave, "no-save", false, "disable session persistence")
	fs.BoolVar(&opts.noInstructions, "no-instructions", false, "do not inject the project's AGENTS.md into the system prompt")
	fs.Func("skills-dir", "directory of Agent Skills to advertise (repeatable); overrides config skills_dirs; default <workdir>/.agents/skills when present", func(v string) error {
		opts.skillsDirs = append(opts.skillsDirs, v)
		return nil
	})
	fs.Func("agent-def-dir", "directory of custom task-tool agent definitions to advertise (repeatable); overrides config agent_defs_dirs; default <workdir>/.agents", func(v string) error {
		opts.agentDefsDirs = append(opts.agentDefsDirs, v)
		return nil
	})
	fs.StringVar(&opts.resume, "r", "", "resume the session with this id")
	fs.StringVar(&opts.resume, "resume", "", "resume the session with this id")
	fs.BoolVar(&opts.cont, "c", false, "continue the most recent session")
	fs.BoolVar(&opts.cont, "continue", false, "continue the most recent session")
	return fs
}

// envInt reads a positive integer from the named environment variable,
// returning 0 (the caller's "use the default" sentinel — see
// server.Options.MaxTaskDepth/MaxConcurrentTasks) when the variable is
// unset, empty, non-numeric, or non-positive. Never errors: a malformed
// value falls back to the default rather than failing serve startup over
// a tuning knob.
func envInt(name string) int {
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// toolConcurrency resolves engine.Config.ToolConcurrency from the two
// operator knobs. The engine never reads an environment variable itself
// (see engine/session_manager.go), so this is where the variables become
// a value.
//
// HARNESS_SEQUENTIAL_TOOLS=1 wins: it is the kill switch that restores
// strictly one-at-a-time tool execution, for a box whose plugin depends
// on the pre-parallel cross-call hook order, or for any workload a
// concurrent batch upsets. HARNESS_TOOL_CONCURRENCY=<n> otherwise sets
// the cap. Neither set leaves 0, which the engine resolves to its own
// default.
func toolConcurrency() int {
	if os.Getenv("HARNESS_SEQUENTIAL_TOOLS") == "1" {
		return 1
	}
	// A NEGATIVE value is handled here rather than through envInt, which
	// folds every non-positive value into 0 (the engine default). That
	// fold would make engine.Config.ToolConcurrency's documented "a
	// negative value is clamped to 1 (sequential)" unreachable through
	// the only operator-facing seam: HARNESS_TOOL_CONCURRENCY=-1 would
	// silently give parallel-at-8 to an operator who asked for the
	// opposite. An explicit negative therefore means sequential, exactly
	// as the field says. 0, empty, and a malformed value still mean "not
	// set" and fall through to the engine default.
	if n, err := strconv.Atoi(os.Getenv("HARNESS_TOOL_CONCURRENCY")); err == nil && n < 0 {
		return 1
	}
	return envInt("HARNESS_TOOL_CONCURRENCY")
}

// toolReadBudgetBytes resolves engine.Config.ToolReadBudgetBytes from
// HARNESS_TOOL_READ_BUDGET_MB. The engine never reads an environment
// variable itself, so this is the seam (same shape as toolConcurrency
// above).
//
// The engine's own default is already safe, so this knob exists for
// tuning, not for turning the bound on: a positive value sets the budget
// in MEGABYTES, an explicit negative value DISABLES the bound for a
// deployment with its own memory discipline, and unset/zero/malformed
// leaves 0 so the engine applies its default.
func toolReadBudgetBytes() int64 {
	raw := os.Getenv("HARNESS_TOOL_READ_BUDGET_MB")
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	if n < 0 {
		// Any negative value means "disabled"; normalize to -1 rather
		// than passing a large negative through as a byte count.
		return -1
	}
	const mib = 1 << 20
	if n > (1<<62)/mib {
		return 0 // absurd; fall back to the engine default
	}
	return n * mib
}

// instructionsMode resolves engine.InstructionsConfig.Mode from the operator
// knob HARNESS_INSTRUCTIONS_MODE and the config key `instructions_mode`, the
// environment variable winning (same shape as instructionsMaxBytes above).
//
// "auto" and an empty value both mean the head-plus-outline rendering for an
// oversize file. Only the exact value "full" selects the head-plus-marker
// rendering; any other value falls back to auto, because an unreadable knob
// must not quietly drop the outline an operator never asked to lose.
func instructionsMode(cfg *config.Config) engine.InstructionsMode {
	raw := os.Getenv("HARNESS_INSTRUCTIONS_MODE")
	if raw == "" {
		raw = cfg.InstructionsMode
	}
	if strings.EqualFold(strings.TrimSpace(raw), string(engine.InstructionsModeFull)) {
		return engine.InstructionsModeFull
	}
	return engine.InstructionsModeAuto
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

// promptSession sends text as an ordinary prompt on s, bracketed by
// sessMgr's turn reporting — see runGoal's identical bracket (and its doc
// comment) for why: without it, a `task` child that finishes while this
// Prompt call is still in flight would find s "idle" from
// SessionManager's point of view and fire a concurrent resume turn on the
// SAME session this call is still driving. resume is fired synchronously
// if non-nil, exactly like runGoal's own tail.
func promptSession(ctx context.Context, sessMgr *engine.SessionManager, s *engine.Session, text string, provenance ...engine.PromptProvenance) error {
	sessMgr.ReportTurnStart(s)
	var msg *message.Message
	var promptErr error
	if len(provenance) > 0 {
		msg, promptErr = s.PromptWithOriginFrom(ctx, text, "", "", provenance[0])
	} else {
		msg, promptErr = s.Prompt(ctx, text)
	}
	resume := sessMgr.ReportTurnEnd(s.ID, msg, promptErr)
	if promptErr != nil {
		return promptErr
	}
	if resume != nil {
		resume()
	}
	return nil
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

// textStreamPrinter renders the plain-text (non -json) engine event stream
// for `harness run`. It prints assistant text deltas to out as they arrive,
// tool starts and failures to errW.
//
// A byte stream cannot retract already-written bytes, so on EventTurnRestart
// — a base-loop retry re-streaming this turn's partial text after a masked
// transient provider error (see engine.EventTurnRestart) — it breaks to a
// fresh line instead of erasing. That keeps the retry's text off the same
// line as the failed attempt's stale partial: "Hello wor\nHello world", never
// the concatenated "Hello worHello world". streamedThis gates the break so an
// attempt that printed no text (a restart before any delta) adds no blank
// line; it resets on EventMessage, the boundary of a completed streamTurn.
type textStreamPrinter struct {
	out  io.Writer
	errW io.Writer
	// mu guards printer state and writes. A child may continue to emit events
	// after the parent prompt returns.
	mu           sync.Mutex
	printedText  bool // any text printed this run; drives the trailing newline
	streamedThis bool // text printed since the last reset; drives the break
}

func (p *textStreamPrinter) handle(ev engine.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch ev.Type {
	case engine.EventTextDelta:
		fmt.Fprint(p.out, ev.Text)
		p.printedText = true
		p.streamedThis = true
	case engine.EventTurnRestart:
		if p.streamedThis {
			fmt.Fprintln(p.out)
			fmt.Fprintln(p.errW, "[re-streaming after a transient provider error]")
			p.streamedThis = false
		}
	case engine.EventMessage:
		p.streamedThis = false
	case engine.EventToolStart:
		fmt.Fprintf(p.errW, "\n[tool %s] %s\n", ev.ToolCall.Name, ev.ToolCall.Arguments)
	case engine.EventToolEnd:
		if ev.IsError {
			fmt.Fprintf(p.errW, "[tool %s failed] %s\n", ev.ToolCall.Name, ev.Output.Text())
		}
	}
}

// PrintedText reports whether handle has ever printed streamed text,
// synchronized against a still-running task child's own handle calls —
// see p.mu's doc comment. runCmd's own tail (the trailing-newline check
// after its top-level Prompt call returns) must go through this rather
// than reading p.printedText directly.
func (p *textStreamPrinter) PrintedText() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.printedText
}

// newRunOnEventHandler builds run mode's engine.Config.OnEvent callback and
// serializes every call. Enabling
// sessMgr in runCmd turns on the `task` tool for run mode, and a `task`
// child's own background Prompt goroutine (SessionManager.Spawn) runs
// CONCURRENTLY with this command's own top-level Prompt/PursueGoal call —
// that concurrency is the entire point of the `task` tool's non-blocking
// contract. Both the parent and the child inherit and call this SAME
// callback (configSnapshot copies Config.OnEvent by value into every
// child's Config), and neither *json.Encoder.Encode nor
// textStreamPrinter.handle (which mutates its own fields and writes
// os.Stdout/os.Stderr) is safe for concurrent use. Before the `task` tool,
// run mode had exactly one session and therefore exactly one goroutine
// ever calling OnEvent; `task` newly exposes it to two (or more, for a
// grandchild). The ReportTurnStart/ReportTurnEnd bracket around runCmd's
// top-level Prompt call does NOT cover this — it only stops SessionManager
// from firing a second concurrent RESUME turn on the parent session; it
// says nothing about a child's own independent Prompt emitting through
// this shared callback at the same time. The server path has no
// equivalent bug: srv.Publish/publishLive route per-SessionID through the
// SSE journal under its own lock — the CLI's callback had no such guard
// until now.
func newRunOnEventHandler(printer *textStreamPrinter, enc *json.Encoder, jsonOut bool) func(engine.Event) {
	var mu sync.Mutex
	return func(ev engine.Event) {
		mu.Lock()
		defer mu.Unlock()
		if jsonOut {
			enc.Encode(ev) //nolint:errcheck
			return
		}
		printer.handle(ev)
	}
}

// modelDecidableBeforeSession reports whether a run's effective model is
// knowable before resolveSession loads or creates a session, and so
// whether an unknown /name's fate (refuse, or defer to a delegated
// session's own vocabulary) can be decided now. A fresh run (neither
// resume nor cont) has no persisted session to disagree with the
// configured model: it IS the effective model. A resumed or continued run
// with an explicit -model (modelSet) also qualifies — resolveSession's
// SetModel lets the flag override the persisted record. Only a resumed or
// continued run without an explicit -model is undecidable this early: its
// persisted model is known only once resolveSession has loaded it.
func modelDecidableBeforeSession(resume string, cont bool, modelSet bool) bool {
	return (resume == "" && !cont) || modelSet
}

func expandRepositoryCommand(cfg *config.Config, workdir, name, line string) (string, bool, error) {
	prompt, err := command.LookupPrompt(commandsDirs(cfg, workdir), name)
	if err != nil || prompt == nil {
		return "", false, err
	}
	body, err := prompt.LoadBody()
	if err != nil {
		return "", false, err
	}
	args := strings.TrimSpace(strings.TrimPrefix(line, "/"+name))
	expanded := command.Expand(body, args)
	if strings.TrimSpace(expanded) == "" {
		return "", false, fmt.Errorf("command: /%s expanded to empty text", name)
	}
	return expanded, true, nil
}

func runCmd(args []string) error {
	// Captured once, at the top of the command, before any flag parsing or
	// session create/load — the ambient engine-identity block's StartedAt
	// (see engine.Config.StartedAt) reports THIS process's start time, not
	// the moment any individual session happened to be created or resumed.
	startedAt := time.Now()
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
	// Registry.Resolve is pure (no I/O, no session), so it runs before any
	// session exists. A command this run will refuse outright — surplus
	// arguments, unsupported in this mode, a frontend command, or a control
	// command with neither -resume nor -continue — must fail here, before
	// resolveSession below builds and prewarms a session for a run that was
	// always going to be refused. Only a command that will actually be
	// dispatched, or ordinary text, reaches resolveSession.
	//
	// An unknown /name is the one exception: harness owns no route for it,
	// but a session delegated to the Claude Code CLI has its own slash
	// vocabulary (/cost, /context, ...) this line might name instead.
	// modelDecidableBeforeSession below decides whether that question can
	// be answered now or must defer to the dispatch switch further down.
	// unknownCmd is nil for every other outcome.
	var res command.Resolution
	var resErr error
	var unknownCmd *command.UnknownCommandError
	if opts.goal == "" {
		res, resErr = command.NewRegistry().Resolve(opts.prompt)
		switch {
		case resErr == nil:
			// unsupported-in-run-mode is reported before the
			// needs-a-session advice: advising -resume for an Op run mode
			// cannot perform at all is a dead end.
			if err := checkRunModeSupport(res); err != nil {
				return err
			}
			if opts.resume == "" && !opts.cont {
				return fmt.Errorf("/%s needs an existing session: pass -resume or -continue, or drop the command and send a plain prompt", resName(res))
			}
		case errors.As(resErr, &unknownCmd):
			// Deferred; see dispatch below.
		case !errors.Is(resErr, command.ErrNotCommand):
			return resErr
		}
	}
	// Structured logging: JSON to stderr, stdlib log/slog only (no new
	// dependency), exactly like serveCmd — built solely to carry the one
	// config-load summary line (see loadConfigLogged); run mode has no
	// other ongoing use for a logger.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := loadConfigLogged(logger)
	if err != nil {
		return err
	}
	// Aliases resolve here; an empty -model falls back to the config's
	// model, then the hard default.
	model, err := message.ParseModelRef(cfg.ResolveModel(opts.model))
	if err != nil {
		return err
	}
	// Refusing here, before resolveSession runs, matters beyond avoiding a
	// wasted session build: on a resumed run resolveSession also calls
	// SetModel, which durably persists a model record — see
	// modelDecidableBeforeSession's doc comment for which runs can decide
	// early enough to refuse before that happens.
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	var promptCommandLine string
	if unknownCmd != nil && opts.resume == "" && !opts.cont {
		expanded, found, err := expandRepositoryCommand(cfg, workDir, unknownCmd.Name, opts.prompt)
		if err != nil {
			return err
		}
		if found {
			promptCommandLine = opts.prompt
			res = command.Resolution{Text: expanded}
			resErr = command.ErrNotCommand
			unknownCmd = nil
		}
	}
	sesDir, err := sessionDir(opts.noSave, cfg.SessionDir)
	if err != nil {
		return err
	}
	if unknownCmd != nil && sesDir != "" && (opts.resume != "" || opts.cont) {
		id := opts.resume
		if opts.cont {
			infos, err := engine.ListSessions(sesDir)
			if err != nil {
				return err
			}
			if len(infos) > 0 {
				id = infos[len(infos)-1].ID
			}
		}
		if id != "" {
			ix, err := engine.ReadSessionIndex(sesDir, id)
			if err == nil {
				commandWorkDir := ix.WorkDir
				if commandWorkDir == "" {
					commandWorkDir = workDir
				}
				expanded, found, err := expandRepositoryCommand(cfg, commandWorkDir, unknownCmd.Name, opts.prompt)
				if err != nil {
					return err
				}
				if found {
					promptCommandLine = opts.prompt
					res = command.Resolution{Text: expanded}
					resErr = command.ErrNotCommand
					unknownCmd = nil
				}
			}
		}
	}
	if unknownCmd != nil && model.Provider != claudecode.Family && modelDecidableBeforeSession(opts.resume, opts.cont, modelSet) {
		return resErr
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

	// run mode keeps the zero-cost-when-unconfigured rule: nil (no
	// `process` tool at all) when the config declares no processes. See
	// buildProcessManager's doc comment for why serve mode differs.
	procMgr := buildProcessManager(workDir, cfg.Processes, false)
	defer closeProcessManager(procMgr)

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
	printer := &textStreamPrinter{out: os.Stdout, errW: os.Stderr}
	onEvent := newRunOnEventHandler(printer, enc, opts.jsonOut)

	// The plugin host's ClientAPI is the direct engine-backed adapter (see
	// cmd/harness/clientapi.go), late-bound: sess is assigned immediately
	// below once resolveSession returns, strictly before the first
	// Prompt/PursueGoal call — the earliest point any hook can fire.
	var sess *engine.Session
	lateAPI.Bind(newLazyRunClientAPI(func() *engine.Session { return sess }))

	// sessMgr enables the `task` tool for `harness run` too, not only
	// `harness serve`. A single-shot run's tree lives and dies with this one
	// process; unlike serveCmd there is no separate wire surface to register
	// children against, so AdoptReloaded below (right after resolveSession)
	// is the only registration point this mode needs.
	sessMgr := engine.NewSessionManager(ctx, envInt("HARNESS_MAX_TASK_DEPTH"), envInt("HARNESS_MAX_CONCURRENT_TASKS"))
	// SetMaxTreeTokens is opt-in. A zero value disables the check.
	sessMgr.SetMaxTreeTokens(envInt("HARNESS_MAX_TREE_TOKENS"))

	s, err := resolveSession(engine.Config{
		Providers: registry(cfg),
		Model:     model,
		System:    systemPrompt(workDir, ""),
		// Config comes first; the per-run flag is the final refinement.
		AppendSystemPrompt:      appendSystemSegments(cfg, opts.system),
		MaxTokens:               opts.maxTokens,
		WorkDir:                 workDir,
		SessionDir:              sesDir,
		SessionSync:             cfg.SessionSync,
		EngineVersion:           version,
		StartedAt:               startedAt,
		OnEvent:                 onEvent,
		OnStorePhase:            slowStorePhaseLogger(logger),
		Instructions:            instructionsConfig(cfg, opts.noInstructions),
		SkillsDirs:              skillsDirs(cfg, opts.skillsDirs, workDir),
		AgentDefsDirs:           agentDefsDirs(cfg, opts.agentDefsDirs, workDir),
		Hooks:                   pluginHooks(host),
		MCP:                     mcpRegistry(mcpMgr),
		MCPToolLoading:          mcpToolLoading(cfg.MCPToolLoading),
		MCPToolLoadingThreshold: cfg.MCPToolLoadingThreshold,
		MCPToolLoadingByServer:  mcpToolLoadingByServer(cfg.MCPServers),
		Processes:               processRegistry(procMgr),
		ContextWindowTokens:     cfg.ContextWindowTokens,
		RequireContextWindow:    cfg.ContextWindowRequiredValue(),
		StreamIdleTimeout:       time.Duration(cfg.StreamIdleTimeoutS) * time.Second,
		PromptRetries:           cfg.PromptRetriesValue(),
		MaxTokensContinuations:  cfg.MaxTokensContinuationsValue(),
		SnapshotEveryRecords:    cfg.SnapshotEveryRecordsValue(),
		CompactionThreshold:     cfg.CompactionThreshold,
		CompactionKeepTurns:     cfg.CompactionKeepTurns,
		// Tool-result retention (config keys tool_result_inline_bytes /
		// tool_result_retained_bytes, product defaults 16384 / 4194304 —
		// see config.ToolResultInlineBytesValue). An explicit <= 0 inline
		// value disables retention; so does an unset sesDir, which the
		// engine checks itself.
		ToolResultInlineBytes:   cfg.ToolResultInlineBytesValue(),
		ToolResultRetainedBytes: cfg.ToolResultRetainedBytesValue(),
		ToolConcurrency:         toolConcurrency(),
		ToolReadBudgetBytes:     toolReadBudgetBytes(),
		// GoalTool mirrors serveCmd's mkCfg below: the `goal` session tool is
		// only useful once an evaluator is actually configured to drive a
		// goal loop against (-goal itself resolves and validates its own
		// evaluator separately, in runGoal below; this just needs to know
		// whether one is configured at all).
		GoalTool: cfg.GoalEvaluatorModel != "",
		// ModelTool is on by default (config `model_tool`, default true); the
		// alias map lets a tool-driven `set` resolve an alias like the CLI's
		// own ResolveModel does.
		ModelTool:      cfg.ModelToolEnabled(),
		ModelAliases:   cfg.Aliases,
		SessionManager: sessMgr,
		ClaudeCode:     claudeCodeConfigFor(cfg, claudecode.Family),
	}, opts.resume, opts.cont, modelSet)
	if err != nil {
		return err
	}
	sess = s
	// The turn below schedules a checkpoint of s at the journal head and
	// writes it in a background goroutine (engine/snapshot.go's on-idle
	// trigger). This process exits the moment runCmd returns, so an
	// unjoined write is a checkpoint racing process teardown: lost, and the
	// next -continue pays the full journal replay the checkpoint exists to
	// bound. The same unjoined write is what makes a caller that deletes
	// the session directory — a test's t.TempDir() cleanup — fail with
	// "directory not empty".
	//
	// It joins s and nothing else, and that limit is real, not an
	// oversight: a `task` child owns its own session, writes its own
	// journal and snapshots under the same directory, and a child that
	// finalizes late can still fire a resume turn on s. A run that spawns
	// children can therefore still have writes in flight at teardown.
	// Draining a whole manager tree is a different problem with a
	// different answer (server.Server's drain), and nothing has reported
	// it for run mode.
	defer s.WaitSnapshots()
	// AdoptReloaded, not AdoptRoot: s.ID may be user-supplied via
	// -resume/-r and could name a FORMER task-tool child from a previous
	// process (its own SessionManager tree, hence its own tree lineage,
	// is gone — this process's sessMgr starts empty) — AdoptRoot would
	// hand it back an unrestricted `task` tool despite that, the same
	// depth-limit bypass AdoptRoot's own doc comment warns about.
	// AdoptReloaded's TaskParentID check correctly falls to the
	// depth-limit-refused case here (this fresh sessMgr never tracks
	// that former parent), a strictly safer default. Errors only on an
	// ID collision (unreachable: s.ID is either freshly minted or
	// restored from a log neither Options field above already
	// registered elsewhere in THIS process), safe to ignore.
	_ = sessMgr.AdoptReloaded(s)
	if unknownCmd != nil && (opts.resume != "" || opts.cont) {
		expanded, found, err := expandRepositoryCommand(cfg, s.WorkDir(), unknownCmd.Name, opts.prompt)
		if err != nil {
			return err
		}
		if found {
			promptCommandLine = opts.prompt
			res = command.Resolution{Text: expanded}
			resErr = command.ErrNotCommand
			unknownCmd = nil
		}
	}

	goalNotAchieved := false
	switch {
	case opts.goal != "":
		res, err := runGoal(ctx, cfg, s, sessMgr, opts)
		if err != nil {
			return err
		}
		goalNotAchieved = !res.Achieved
	case unknownCmd != nil && s.ClaudeCodeDelegated():
		// The CLI advertises its own slash commands (slash_commands in its
		// stream-json init line) and reports its own error for a name it
		// does not know either. Send
		// opts.prompt, the ORIGINAL line (e.g. "/cost"), not res.Text —
		// Resolve set no Text for an unknown command, and the CLI expects
		// its own leading slash.
		if err := promptSession(ctx, sessMgr, s, opts.prompt); err != nil {
			return err
		}
	case unknownCmd != nil:
		return resErr
	case resErr == nil:
		// res was already resolved and refusal-checked above, before s was
		// built: only a command that will actually run reaches here.
		if derr := dispatchCommand(ctx, s, res); derr != nil {
			return derr
		}
	default:
		if promptCommandLine != "" {
			label, err := server.SanitizeSourceLabel(promptCommandLine)
			if err != nil {
				return err
			}
			if err := promptSession(ctx, sessMgr, s, res.Text, engine.PromptProvenance{
				Source: message.PromptSourceCommand, SourceLabel: label,
			}); err != nil {
				return err
			}
		} else if err := promptSession(ctx, sessMgr, s, res.Text); err != nil {
			return err
		}
	}
	if printer.PrintedText() {
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
//
// Brackets the PursueGoal call with sessMgr.ReportTurnStart/ReportTurnEnd
// — see runCmd's identical bracket around its own bare s.Prompt call for
// why: `harness run` never installs an engine.ExternalRunner (that only
// exists for server.Server's own run-slot admission), so without this
// bracket SessionManager's view of s never leaves StatusIdle for the
// whole PursueGoal call. A `task` child spawned mid-goal that finishes
// fast would then find s "idle" and fire a CONCURRENT resume turn via
// triggerResumeLocked's no-ExternalRunner fallback (a direct s.Prompt
// call) while THIS PursueGoal call is still driving s — two goroutines
// calling Session.Prompt/PursueGoal on the same session at once, the
// exact contract violation ExternalRunner prevents for the server.
func runGoal(ctx context.Context, cfg *config.Config, s *engine.Session, sessMgr *engine.SessionManager, opts runOptions) (*engine.GoalResult, error) {
	if cfg.GoalEvaluatorModel == "" {
		return nil, fmt.Errorf("goal_evaluator_model must be set in config to use -goal")
	}
	evaluator, err := message.ParseModelRef(cfg.ResolveModel(cfg.GoalEvaluatorModel))
	if err != nil {
		return nil, fmt.Errorf("goal_evaluator_model: %w", err)
	}
	sessMgr.ReportTurnStart(s)
	res, err := s.PursueGoal(ctx, opts.goal, engine.GoalOptions{
		MaxTurns:  opts.goalMaxTurns,
		Evaluator: evaluator,
	})
	// resume: see runCmd's identical variable for why it is fired
	// synchronously, right here, rather than left unfired — this
	// process has no HTTP request to keep serving and no run-slot to
	// release first; ReportTurnEnd only needs to run after PursueGoal
	// itself has fully returned, which it just did.
	resume := sessMgr.ReportTurnEnd(s.ID, nil, err)
	if err != nil {
		return nil, err
	}
	if res.Achieved {
		fmt.Fprintf(os.Stderr, "goal achieved in %d turn(s): %s\n", res.Turns, res.Reason)
	} else {
		fmt.Fprintf(os.Stderr, "goal not achieved after %d turn(s): %s\n", res.Turns, res.Reason)
	}
	if resume != nil {
		resume()
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
// providers entry with type "openai-compat" or "openai" needs no code at
// all — see registerOpenAICompatProviders and registerOpenAIProviders —
// and OpenRouter itself needs no config entry either, see
// ensureDefaultOpenRouter.
//
// registry does not assume cfg came from config.LoadProject (the load path
// that guarantees config.Defaults().Providers fields are filled in — see
// config.EnsureProviderDefaults): it calls EnsureProviderDefaults itself
// first, idempotently, so a hand-built *config.Config (as tests use, and
// any future embedder that skips LoadProject might too) resolves a minimal
// {"openrouter": {"api_key_env": "..."}} entry identically to one that went
// through the full config-loading choke point, rather than silently
// registering no adapter for it at all.
// defaultOpenAIKeyEnv is the environment variable every provider/openai
// client reads when its entry names none of its own. One constant, so the
// built-in entry (providerAuth, above) and a configured type:"openai" entry
// (registerOpenAIProviders) cannot drift to different defaults.
const defaultOpenAIKeyEnv = "OPENAI_API_KEY"

func registry(cfg *config.Config) provider.Registry {
	if cfg != nil {
		config.EnsureProviderDefaults(cfg.Providers)
	}
	akey, abase := providerAuth(cfg, anthropic.Family, "ANTHROPIC_API_KEY")
	okey, obase := providerAuth(cfg, openai.Family, defaultOpenAIKeyEnv)
	reg := provider.Registry{
		anthropic.Family: &anthropic.Client{APIKey: akey, BaseURL: abase, CacheTTL: anthropicCacheTTL(cfg), ExtraHeaders: nativeExtraHeaders(cfg, anthropic.Family)},
		// The built-in openai entry deliberately leaves Family empty: it IS
		// the package default, and naming it here would only invite the two
		// to drift. Its ResponsesPath/OmitResponseParams/
		// SanitizeToolSchemas/UseWebSocketTransport come from the native
		// entry, if any.
		openai.Family: &openai.Client{APIKey: okey, BaseURL: obase, ExtraHeaders: nativeExtraHeaders(cfg, openai.Family), ResponsesPath: nativeResponsesPath(cfg), OmitResponseParams: nativeOmitResponseParams(cfg), SanitizeToolSchemas: nativeSanitizeToolSchemas(cfg), UseWebSocketTransport: nativeUseWebSocketTransport(cfg)},
	}
	registerOpenAICompatProviders(reg, cfg)
	registerOpenAIProviders(reg, cfg)
	registerClaudeCodeProviders(reg, cfg)
	ensureDefaultOpenRouter(reg, cfg)
	return reg
}

// registerClaudeCodeProviders registers a claudecode.Client — a
// provider.Provider stand-in never expected to actually stream, see that
// package's own doc comment — for every config.Providers entry of
// config.TypeClaudeCodeCLI, keyed by its providers map name, exactly like
// registerOpenAICompatProviders. This is what makes Session.ModelSupported
// (engine/engine.go, consulted by the `model` tool, POST
// /session/{id}/model, and Spawn's model-override validation) accept a
// swap to a claude-code model ref: without an entry in the registry under
// that key, ModelSupported would reject the very refs
// engine.ClaudeCodeProviderFamily's delegated-turn dispatch exists to
// serve, even though that dispatch never actually calls into this
// registered client's Stream method.
func registerClaudeCodeProviders(reg provider.Registry, cfg *config.Config) {
	if cfg == nil {
		return
	}
	for name, p := range cfg.Providers {
		if p.Type != config.TypeClaudeCodeCLI {
			continue
		}
		reg[name] = claudecode.Client{}
	}
}

// claudeCodeConfigFor resolves the engine.ClaudeCodeConfig for the given
// providers-map key (by convention claudecode.Family, "claude-code") from a
// config.TypeClaudeCodeCLI entry, translating config.Provider's
// BinaryPath/ExtraArgs/PermissionMode fields into their engine.Config
// counterpart. Absent (no such entry, or cfg nil) yields the zero value,
// which engine.newSession defaults BinaryPath from ("claude" — see that
// function). Package engine deliberately does not import package config
// (see engine.Config's own field-by-field translation precedent, e.g.
// SessionSync/ContextWindowTokens above), so this narrow translation lives
// here, at the one boundary that already does it for every other field.
func claudeCodeConfigFor(cfg *config.Config, name string) engine.ClaudeCodeConfig {
	if cfg == nil {
		return engine.ClaudeCodeConfig{}
	}
	p, ok := cfg.Providers[name]
	if !ok || p.Type != config.TypeClaudeCodeCLI {
		return engine.ClaudeCodeConfig{}
	}
	return engine.ClaudeCodeConfig{
		BinaryPath:     p.BinaryPath,
		ExtraArgs:      p.ExtraArgs,
		PermissionMode: p.PermissionMode,
	}
}

// registerOpenAIProviders builds a native provider/openai (Responses API)
// client for every config.Providers entry of config.TypeOpenAI, keyed by
// its providers map name — that name is what routes "name/model" refs to
// it, exactly like an openai-compat entry, and it is also the client's own
// Family, so the entry's opaque reasoning attachments are tagged and
// replayed under the key that identifies its endpoint rather than under the
// shared package constant.
//
// Registration order IS a precedence rule, not a formality, and the earlier
// version of this comment claimed otherwise. config.validateProviders does
// NOT reject an entry that collides with a built-in key: type:"openai" is
// valid under ANY map key, including "openai" and "anthropic". What it
// rejects is an unknown type, and an empty type on a key that is neither
// native nor native-default.
//
// So the guarantee is narrower and comes from the map, not from validation:
// a providers map key has exactly one entry, hence exactly one type, so
// registerOpenAICompatProviders and this function can never both claim the
// same key. Where an entry names a built-in key, it deliberately REPLACES
// the built-in adapter, and running last is what makes the explicit entry
// win. Its API key falls back to the same environment variable the built-in
// entry reads, so replacing the built-in this way never silently
// unauthenticates it.
func registerOpenAIProviders(reg provider.Registry, cfg *config.Config) {
	if cfg == nil {
		return
	}
	for name, p := range cfg.Providers {
		if p.Type != config.TypeOpenAI {
			continue
		}
		// An entry that names no api_key_env is asking for this adapter's
		// DEFAULT key source, not for no key: the built-in "openai" entry
		// has always read defaultOpenAIKeyEnv (providerAuth), and an entry
		// keyed "openai" replaces that client outright. Without the same
		// fallback, adding a type to an existing entry would silently
		// unauthenticate every request it makes. A deployment that must
		// keep its OpenAI key away from a third-party endpoint names its
		// own api_key_env, which wins here — and an unset named variable
		// resolves empty rather than falling back, so naming a variable is
		// always the stricter choice.
		keyEnv := p.APIKeyEnv
		if keyEnv == "" {
			keyEnv = defaultOpenAIKeyEnv
		}
		apiKey := os.Getenv(keyEnv)
		reg[name] = &openai.Client{
			Family:                name,
			APIKey:                apiKey,
			BaseURL:               p.BaseURL,
			ExtraHeaders:          p.ExtraHeaders,
			ResponsesPath:         p.ResponsesPath,
			OmitResponseParams:    p.OmitResponseParams,
			SanitizeToolSchemas:   p.SanitizeToolSchemas,
			UseWebSocketTransport: p.UseWebSocketTransport,
		}
	}
}

// nativeResponsesPath reads the request path configured on the NATIVE
// "openai" entry (map key "openai", no type — the shape
// config.validateProviders permits responses_path on). Empty leaves the
// adapter's own /v1/responses default in place. A keyed type:"openai" entry
// carries its own path and is wired by registerOpenAIProviders instead.
func nativeResponsesPath(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	p := cfg.Providers[openai.Family]
	if p.Type != "" {
		return ""
	}
	return p.ResponsesPath
}

// nativeOmitResponseParams reads omit_response_params configured on the
// NATIVE "openai" entry (map key "openai", no type — the same shape
// config.validateProviders permits omit_response_params on). Empty leaves
// the adapter sending every param it always has. A keyed type:"openai"
// entry carries its own list and is wired by registerOpenAIProviders
// instead. Mirrors nativeResponsesPath exactly.
func nativeOmitResponseParams(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	p := cfg.Providers[openai.Family]
	if p.Type != "" {
		return nil
	}
	return p.OmitResponseParams
}

// nativeExtraHeaders reads extra_headers configured on a NATIVE provider
// entry (the map key itself, no type). A keyed type:"..." entry carries its
// own value and is wired by its own register* function instead. Mirrors
// nativeOmitResponseParams.
func nativeExtraHeaders(cfg *config.Config, family string) map[string]string {
	if cfg == nil {
		return nil
	}
	p := cfg.Providers[family]
	if p.Type != "" {
		return nil
	}
	return p.ExtraHeaders
}

// nativeSanitizeToolSchemas reads sanitize_tool_schemas configured on the
// NATIVE "openai" entry (map key "openai", no type — the same shape
// config.validateProviders permits sanitize_tool_schemas on). false leaves
// the adapter sending every tool schema unchanged. A keyed type:"openai"
// entry carries its own value and is wired by registerOpenAIProviders
// instead. Mirrors nativeOmitResponseParams exactly.
func nativeSanitizeToolSchemas(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	p := cfg.Providers[openai.Family]
	if p.Type != "" {
		return false
	}
	return p.SanitizeToolSchemas
}

// nativeUseWebSocketTransport reads use_websocket_transport configured on
// the NATIVE "openai" entry (map key "openai", no type — the same shape
// config.validateProviders permits use_websocket_transport on). false
// leaves the adapter on the HTTP + SSE transport. A keyed type:"openai"
// entry carries its own value and is wired by registerOpenAIProviders
// instead. Mirrors nativeOmitResponseParams exactly.
func nativeUseWebSocketTransport(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	p := cfg.Providers[openai.Family]
	if p.Type != "" {
		return false
	}
	return p.UseWebSocketTransport
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
		Family:           family,
		APIKey:           apiKey,
		BaseURL:          p.BaseURL,
		ExtraHeaders:     p.ExtraHeaders,
		NoPromptCacheKey: p.NoPromptCacheKey,
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

// anthropicCacheTTL reads the configured prompt-cache TTL for the native
// anthropic entry. Empty (no config, or no cache_ttl key) leaves the
// adapter's own DefaultCacheTTL in place; config.validateProviders has
// already rejected any other value on the load path.
func anthropicCacheTTL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Providers[anthropic.Family].CacheTTL
}

// instructionsConfig translates the -no-instructions flag and config file
// fields into the engine's InstructionsConfig. Precedence: the flag disables
// unconditionally; otherwise config `instructions: false` disables, config
// `instructions_path` names an override, `instructions_max_bytes` (or
// HARNESS_INSTRUCTIONS_MAX_KB) sets the injection cap, and a config that sets
// none of them returns nil (the engine default: auto-discover AGENTS.md by
// walking up from WorkDir, capped at 64 KiB).
func instructionsConfig(cfg *config.Config, noInstructions bool) *engine.InstructionsConfig {
	if noInstructions {
		return &engine.InstructionsConfig{Disabled: true}
	}
	if cfg == nil {
		// A nil config still honors the environment knob: the caller has no
		// config file, not a demand for the default cap.
		cfg = &config.Config{}
	}
	if cfg.Instructions != nil && !*cfg.Instructions {
		return &engine.InstructionsConfig{Disabled: true}
	}
	maxBytes := instructionsMaxBytes(cfg)
	mode := instructionsMode(cfg)
	if cfg.InstructionsPath == "" && maxBytes == 0 && mode == engine.InstructionsModeAuto {
		return nil
	}
	return &engine.InstructionsConfig{Path: cfg.InstructionsPath, MaxBytes: maxBytes, Mode: mode}
}

// instructionsMaxBytes resolves engine.InstructionsConfig.MaxBytes from the
// operator knob HARNESS_INSTRUCTIONS_MAX_KB and the config key
// `instructions_max_bytes`. The engine never reads an environment variable
// itself, so this is the seam (same shape as toolReadBudgetBytes above).
//
// The environment variable counts KILOBYTES, because an instruction file is
// a human-sized document and the engine default reads as "64 KiB" everywhere.
// A positive value sets the cap, a NEGATIVE value disables it (the whole file
// is injected), and unset/zero/malformed falls through to the config key.
// Zero from both leaves the engine default of 64 KiB.
func instructionsMaxBytes(cfg *config.Config) int {
	raw := os.Getenv("HARNESS_INSTRUCTIONS_MAX_KB")
	if n, err := strconv.Atoi(raw); err == nil && n != 0 {
		if n < 0 {
			// Any negative value means "no cap"; normalize to -1 rather
			// than passing a large negative through as a byte count.
			return -1
		}
		const kib = 1 << 10
		if n > (1<<62)/kib {
			return 0 // absurd; fall back to the config key or the default
		}
		return n * kib
	}
	if cfg.InstructionsMaxBytes < 0 {
		return -1
	}
	return cfg.InstructionsMaxBytes
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

// agentDefsDirs resolves the effective custom-agent-definition directories
// for the engine. Repeatable -agent-def-dir flags override
// config agent_defs_dirs entirely; otherwise config agent_defs_dirs is used.
// Relative entries resolve against workDir. When neither is set it returns
// nil, leaving the engine default in place (use <workDir>/.agents).
func agentDefsDirs(cfg *config.Config, flagDirs []string, workDir string) []string {
	dirs := flagDirs
	if len(dirs) == 0 && cfg != nil && cfg.AgentDefsDirs != nil {
		// A config file's explicit "agent_defs_dirs": [] is an opt-out and
		// must stay a non-nil empty slice; only a truly absent field falls
		// through to nil (engine default discovery).
		dirs = cfg.AgentDefsDirs
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

func commandsDirs(cfg *config.Config, workDir string) []string {
	var dirs []string
	if cfg != nil {
		dirs = cfg.CommandsDirs
	}
	if dirs == nil {
		dirs = []string{filepath.Join(workDir, ".agents", "commands")}
	}
	if len(dirs) == 0 {
		return []string{}
	}
	out := make([]string, len(dirs))
	for i, dir := range dirs {
		if filepath.IsAbs(dir) {
			out[i] = dir
		} else {
			out[i] = filepath.Join(workDir, dir)
		}
	}
	return out
}

// appendSystemSegments returns config segments followed by the per-run flag.
// The result does not alias the config slice.
func appendSystemSegments(cfg *config.Config, extra string) []string {
	var segs []string
	if cfg != nil && len(cfg.AppendSystemPrompt) > 0 {
		segs = append(segs, cfg.AppendSystemPrompt...)
	}
	if extra != "" {
		segs = append(segs, extra)
	}
	return segs
}

func systemPrompt(workDir, extra string) []string {
	system := []string{prompt.EngineBase(workDir)}
	if extra != "" {
		system = append(system, extra)
	}
	return system
}
