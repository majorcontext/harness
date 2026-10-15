package e2e

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// fakeClock is the clock of the injected logins of the in-process host. A step moves it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// acpInjectedAdvance moves the clock of the injected logins.
func acpInjectedAdvance(d time.Duration) acpCheck {
	return func(t *testing.T, c *acpConn) { c.env.clock.Advance(d) }
}

const injectedLoginURL = "https://console.example/settings/codex"

var injectedInit = acpInit{version: 1, want: 1, elicitationURL: true, methods: []string{"chatgpt", "injected-codex"}}

// acpInjectedElicit waits for the URL elicitation of an injected sign-in and checks its fields against the authenticate request kept under auth.
type acpInjectedElicit struct{ as, auth string }

func (s acpInjectedElicit) run(t *testing.T, c *acpConn) {
	t.Helper()
	acpAwaitElicitation{as: s.as, check: func(t *testing.T, p map[string]any) {
		id, _ := p["elicitationId"].(string)
		switch {
		case p["mode"] != "url", p["url"] != injectedLoginURL:
			t.Errorf("elicitation mode and url = %v %v, want url %s", p["mode"], p["url"], injectedLoginURL)
		case p["requestId"] != float64(c.pending[s.auth]):
			t.Errorf("elicitation requestId = %v, want the authenticate request %d", p["requestId"], c.pending[s.auth])
		case id == "":
			t.Error("elicitation has no elicitationId")
		case p["message"] != "Sign in on the page that opens, then return here.":
			t.Errorf("elicitation message = %q", p["message"])
		}
		if _, ok := p["sessionId"]; ok {
			t.Error("elicitation of a sign-in names a session")
		}
		c.paths[id] = "<elicitation:" + s.as + ">"
		if c.env.injectedElicitations == nil {
			c.env.injectedElicitations = map[string]string{}
		}
		c.env.injectedElicitations[s.as] = id
	}}.run(t, c)
}

// acpInjectedComplete waits for the completion of the elicitation kept under as.
type acpInjectedComplete struct{ as string }

func (s acpInjectedComplete) run(t *testing.T, c *acpConn) {
	t.Helper()
	want := c.env.injectedElicitations[s.as]
	f := c.awaitMessage(func(m map[string]any) bool { return m["method"] == "elicitation/complete" })
	if p, _ := f["params"].(map[string]any); p["elicitationId"] != want {
		t.Errorf("completed elicitation = %v, want %s", p["elicitationId"], want)
	}
}

// acpInjectedSignIn makes the fake server accept requests, as the login page does for the user.
func acpInjectedSignIn(t *testing.T, c *acpConn) {
	if l := c.env.login; l.openai != nil {
		l.openai.SignIn()
		return
	}
	c.env.login.fake.SignIn()
}

// acpInjectedDenials checks how many requests the fake server denied.
func acpInjectedDenials(want int) acpCheck {
	return func(t *testing.T, c *acpConn) {
		t.Helper()
		if got := c.env.login.openai.Denials(); got != want {
			t.Errorf("denied requests = %d, want %d", got, want)
		}
	}
}

// acpInjectedNoElicitation fails when the agent sent an elicitation.
func acpInjectedNoElicitation(t *testing.T, c *acpConn) {
	t.Helper()
	for _, f := range c.wire {
		if f.Dir == "agent" && strings.Contains(string(f.Frame), `"elicitation/`) {
			t.Errorf("the agent sent an elicitation frame: %s", f.Frame)
		}
	}
}

const (
	injectedMethodCite = "`initialize` offers the method `injected-<provider>`, \"Log in to <provider>\", of type `agent`, for each provider entry of the user config with `auth` `injected` and a `login_url`, and only to a client that advertises `elicitation.url`."
	injectedAuthCite   = "`authenticate` with the method `injected-<provider>` sends the status probe first and answers `{}` at once when it answers 200."
	injectedElicitCite = "Otherwise it sends `elicitation/create` with `mode` `url`, no `sessionId`, `requestId` the id of the `authenticate` request, a fresh `elicitationId`, `url` the `login_url`, and `message` \"Sign in on the page that opens, then return here.\""
	injectedPollCite   = "After `accept`, the agent sends the probe at once, then every 2 s (every 4 s after 30 s, every 8 s after 2 minutes), for at most 10 minutes. When the probe answers 200, the agent sends `elicitation/complete` with the `elicitationId` and answers `{}`."
	injectedCheckCite  = "For an entry with `auth` `injected`, `session/new`, `session/load`, and `session/resume` send the status probe and fail with `-32000`, `data.code` `login_required`, when it answers 401."
	injectedCacheCite  = "`session/prompt` sends no probe and reads the cached answer only. The answer is cached for 30 s, and it is read again after `authenticate` and after a failed turn."
	injectedPermCite   = "A 401 of a model call within 2 minutes after a probe of the same entry answered 200 ends the turn `permanent`, with the message `the server accepted the sign-in check but rejected the model call; sign in again at <login_url>, or check how the server authorizes model calls`"
	injected401Cite    = "A 401 of an entry with `auth` `injected` fails the turn with the error class `login_required`."
	injected403Cite    = "A 403 is not `login_required`."
	injectedLateCite   = "A 401 of a model call 2 minutes or more after the last probe that answered 200 fails the turn with the error class `login_required`."
	injectedTimeout    = "After `accept`, the agent sends the probe at once, then every 2 s (every 4 s after 30 s, every 8 s after 2 minutes), for at most 10 minutes."
	injectedBound      = "so a client that never answers the elicitation cannot hold the request open."
	injectedNoProbe    = "A probe that answers 404 or 405 means that the server has no status probe: `authenticate` then answers `{}` when the user accepts the elicitation, and only the 401 of a turn is `login_required`."
	injectedProbeCite  = "The status probe of the Anthropic adapter is `GET <base_url>/v1/models` with the header `Anthropic-Version`."
)

var acpInjectedScenarios = []acpScenario{
	{
		name:  "initialize_offers_the_injected_method_to_a_client_with_url_elicitation",
		cite:  injectedMethodCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{}},
		steps: []acpStep{injectedInit},
	},
	{
		name:  "initialize_offers_no_injected_method_without_url_elicitation",
		cite:  injectedMethodCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{}},
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitation: true, methods: []string{"chatgpt"}},
			acpCall{method: "authenticate", params: map[string]any{"methodId": "injected-codex"}, code: acpInvalidParams},
		},
	},
	{
		name:  "initialize_offers_no_injected_method_without_a_login_url",
		cite:  injectedMethodCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{}, injectedNoLoginURL: true},
		steps: []acpStep{acpInit{version: 1, want: 1, elicitationURL: true, methods: []string{"chatgpt"}}},
	},
	{
		name:  "authenticate_injected_answers_at_once_when_the_probe_answers_200",
		cite:  injectedAuthCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{}},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpAuthDone{id: "auth"},
			acpCheck(acpInjectedNoElicitation),
		},
	},
	{
		name:  "authenticate_injected_elicits_the_login_url_and_completes_after_the_sign_in",
		cite:  injectedElicitCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpCheck(acpInjectedSignIn),
			acpAnswerElicitation{as: "login", action: "accept"},
			acpAuthDone{id: "auth"},
			acpInjectedComplete{as: "login"},
			acpCheck(acpDeviceCompletionFirst("auth")),
		},
	},
	{
		name:  "authenticate_injected_polls_the_probe_after_accept",
		cite:  injectedPollCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "accept"},
			acpCheck(acpInjectedSignIn),
			acpAuthDone{id: "auth"},
			acpInjectedComplete{as: "login"},
		},
	},
	{
		name:  "declined_injected_elicitation_ends_the_sign_in",
		cite:  declineCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "decline"},
			acpAuthDone{id: "auth", code: acpInternal},
		},
	},
	{
		name:  "cancelled_injected_elicitation_ends_the_sign_in",
		cite:  declineCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "cancel"},
			acpAuthDone{id: "auth", code: acpRequestCanceled},
		},
	},
	{
		name:  "cancel_request_during_injected_authenticate_cancels_the_elicitation",
		cite:  "and the agent sends `$/cancel_request` for an elicitation that has no answer.",
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpCancelRequest{id: "auth"},
			acpAuthDone{id: "auth", code: acpRequestCanceled},
			acpAwaitCancelRequest{as: "login"},
		},
	},
	{
		name:  "second_authenticate_cancels_the_injected_sign_in_in_flight",
		cite:  "A new `authenticate` cancels the one in flight, whatever the method, and answers `-32800` to it.",
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "first", method: "injected-codex"},
			acpInjectedElicit{as: "one", auth: "first"},
			acpAuthStart{id: "second", method: "injected-codex"},
			acpAuthDone{id: "first", code: acpRequestCanceled},
			acpAwaitCancelRequest{as: "one"},
			acpInjectedElicit{as: "two", auth: "second"},
			acpCheck(acpInjectedSignIn),
			acpAnswerElicitation{as: "two", action: "accept"},
			acpAuthDone{id: "second"},
			acpInjectedComplete{as: "two"},
		},
	},
	{
		name:  "new_session_with_a_401_probe_is_auth_required_with_the_login_url",
		cite:  injectedCheckCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		steps: []acpStep{injectedInit, acpNew{as: "a", code: acpAuthRequired}},
	},
	{
		name:  "new_session_with_a_401_probe_and_no_login_url_names_the_provider_only",
		cite:  injectedCheckCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}, injectedNoLoginURL: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitationURL: true, methods: []string{"chatgpt"}},
			acpNew{as: "a", code: acpAuthRequired},
		},
	},
	{
		name:  "new_session_with_an_unknown_probe_is_allowed",
		cite:  "A probe that fails or takes more than 3 s is unknown and raises no error.",
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, ProbeStatus: 503}},
		steps: []acpStep{injectedInit, acpNew{as: "a"}},
	},
	{
		name:  "resume_session_with_a_401_probe_is_auth_required",
		cite:  injectedCheckCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, ProbeStatus: 503}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpCheck(func(t *testing.T, c *acpConn) { c.env.login.openai.SetProbeStatus(0) }),
			acpLoad{as: "a", resume: true, code: acpAuthRequired},
			acpLoad{as: "a", code: acpAuthRequired},
		},
	},
	{
		name:  "new_session_reads_the_cached_probe_for_30_seconds",
		cite:  injectedCacheCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpNew{as: "b"},
			acpCheck(func(t *testing.T, c *acpConn) {
				// One probe, and one fetch of the model list that Runtime.Models makes.
				if n := c.env.login.openai.ModelRequests(); n != 2 {
					t.Errorf("model list requests = %d, want 2", n)
				}
			}),
		},
	},
	{
		name:  "prompt_401_is_auth_required_and_the_next_prompt_reads_the_cache",
		cite:  injected401Cite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, ProbeStatus: 503}, claudeFrames: nil},
		model: []harnesstest.Step{{Name: "reply", Repeat: true, Reply: codexText("hi")}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", code: acpAuthRequired, dataCode: "login_required"},
			acpPrompt{as: "a", text: "two", code: acpAuthRequired, dataCode: "login_required"},
			acpCheck(acpInjectedDenials(3)),
		},
	},
	{
		name:  "prompt_401_after_a_200_probe_ends_the_turn_permanent_and_authenticate_does_not_loop",
		cite:  injectedPermCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, WeakProbe: true}},
		model: []harnesstest.Step{{Name: "reply", Repeat: true, Reply: codexText("hi")}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", code: acpInternal, class: "permanent"},
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpAuthDone{id: "auth"},
			acpPrompt{as: "a", text: "two", code: acpInternal, class: "permanent"},
			acpCheck(acpInjectedDenials(2)),
		},
	},
	{
		name:  "prompt_403_is_not_auth_required",
		cite:  injected403Cite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, DenyStatus: 403, ProbeStatus: 503}},
		model: []harnesstest.Step{{Name: "reply", Repeat: true, Reply: codexText("hi")}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", code: acpInternal, class: "unrecovered"},
		},
	},
	{
		name:  "prompt_after_the_sign_in_succeeds",
		cite:  injectedAuthCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}},
		model: codexReplyHi,
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a", code: acpAuthRequired},
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpCheck(acpInjectedSignIn),
			acpAnswerElicitation{as: "login", action: "accept"},
			acpAuthDone{id: "auth"},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hello", stop: "end_turn"},
		},
	},
	{
		name:  "prompt_401_two_minutes_after_a_200_probe_is_auth_required",
		cite:  injectedLateCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, WeakProbe: true}, clock: true},
		model: []harnesstest.Step{{Name: "reply", Repeat: true, Reply: codexText("hi")}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpCheck(acpInjectedAdvance(2 * time.Minute)),
			acpPrompt{as: "a", text: "one", code: acpAuthRequired, dataCode: "login_required"},
		},
	},
	{
		name:  "authenticate_injected_ends_after_10_minutes_without_a_sign_in",
		cite:  injectedTimeout,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}, clock: true},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "accept"},
			acpCheck(func(t *testing.T, c *acpConn) {
				testpoll.Until(t, waitBound, "the first poll after the accept did not come", func() bool { return c.env.login.openai.Denials() >= 2 })
				c.env.clock.Advance(10 * time.Minute)
			}),
			acpAuthDone{id: "auth", code: acpInternal},
		},
	},
	{
		name:  "authenticate_injected_ends_when_the_client_never_answers_the_elicitation",
		cite:  injectedBound,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true}, clock: true, budget: 300 * time.Millisecond},
		steps: []acpStep{
			injectedInit,
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpAuthDone{id: "auth", code: acpInternal},
			acpAwaitCancelRequest{as: "login"},
		},
	},
	{
		name:  "authenticate_injected_with_no_status_probe_answers_after_accept",
		cite:  injectedNoProbe,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, ProbeStatus: 404}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpAuthStart{id: "auth", method: "injected-codex"},
			acpInjectedElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "accept"},
			acpAuthDone{id: "auth"},
			acpInjectedComplete{as: "login"},
		},
	},
	{
		name:  "prompt_401_with_no_status_probe_is_auth_required",
		cite:  injectedNoProbe,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, ProbeStatus: 405}},
		model: []harnesstest.Step{{Name: "reply", Repeat: true, Reply: codexText("hi")}},
		steps: []acpStep{
			injectedInit,
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", code: acpAuthRequired, dataCode: "login_required"},
		},
	},
	{
		name:  "initialize_offers_the_injected_method_of_the_anthropic_entry_and_the_probe_sends_the_version",
		cite:  injectedProbeCite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{}, anthropic: true},
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitationURL: true, methods: []string{"chatgpt", "injected-anthropic"}},
			acpNew{as: "a"},
			acpCheck(func(t *testing.T, c *acpConn) {
				got := c.env.login.fake.InjectedRequests()
				i := slices.IndexFunc(got, func(r harnesstest.InjectedRequest) bool { return r.Method == "GET" && r.Path == "/v1/models" })
				switch {
				case i < 0:
					t.Fatalf("requests = %v, want the probe GET /v1/models", got)
				case got[i].Version != "2023-06-01", len(got[i].CredentialHeaders) != 0:
					t.Errorf("probe = %+v, want the version and no credential header", got[i])
				}
			}),
		},
	},
	{
		name:  "anthropic_injected_prompt_401_is_auth_required",
		cite:  injected401Cite,
		login: &acpLoginSpec{injected: &harnesstest.InjectedOptions{SignedOut: true, ProbeStatus: 503}, anthropic: true},
		model: []harnesstest.Step{{Name: "reply", Repeat: true, Reply: harnesstest.Reply{Text: "hi"}}},
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitationURL: true, methods: []string{"chatgpt", "injected-anthropic"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", code: acpAuthRequired, dataCode: "login_required"},
		},
	},
}
