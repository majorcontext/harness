package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// deviceSpec scripts the issuer of the device code login of a wasm host.
type deviceSpec struct {
	// pending is the number of polls that answer that the user has not entered the code. slowDown is the number of leading polls that answer slow_down.
	pending, slowDown int
}

// deviceRun is a started deviceSpec.
type deviceRun struct {
	issuer *harnesstest.CodexIssuer
	// elicitations maps a step name to the elicitationId of the URL elicitation that the step took.
	elicitations map[string]string
}

func startDevice(t *testing.T, spec deviceSpec) *deviceRun {
	t.Helper()
	return &deviceRun{issuer: harnesstest.NewCodexIssuer(t, harnesstest.CodexIssuerOptions{DevicePending: spec.pending, SlowDown: spec.slowDown}), elicitations: map[string]string{}}
}

// register gives the connection the placeholder of the issuer URL.
func (d *deviceRun) register(c *acpConn) {
	if d != nil {
		c.paths[d.issuer.URL()] = "<issuer>"
	}
}

// acpDeviceElicit waits for the URL elicitation of a sign-in and checks its fields against the authenticate request kept under auth.
type acpDeviceElicit struct{ as, auth string }

// acpDeviceComplete waits for the completion of the elicitation kept under as.
type acpDeviceComplete struct{ as string }

func (s acpDeviceElicit) run(t *testing.T, c *acpConn) {
	t.Helper()
	d := c.env.device
	acpAwaitElicitation{as: s.as, check: func(t *testing.T, p map[string]any) {
		want := d.issuer.URL() + "/codex/device?user_code=" + harnesstest.CodexUserCode
		id, _ := p["elicitationId"].(string)
		switch {
		case p["mode"] != "url", p["url"] != want:
			t.Errorf("elicitation mode and url = %v %v, want url %s", p["mode"], p["url"], want)
		case p["requestId"] != float64(c.pending[s.auth]):
			t.Errorf("elicitation requestId = %v, want the authenticate request %d", p["requestId"], c.pending[s.auth])
		case id == "":
			t.Error("elicitation has no elicitationId")
		case !strings.Contains(p["message"].(string), harnesstest.CodexUserCode):
			t.Errorf("elicitation message = %q, want it to name the code", p["message"])
		}
		if _, ok := p["sessionId"]; ok {
			t.Error("elicitation of a sign-in names a session")
		}
		d.elicitations[s.as] = id
		c.paths[id] = "<elicitation:" + s.as + ">"
	}}.run(t, c)
}

func (s acpDeviceComplete) run(t *testing.T, c *acpConn) {
	t.Helper()
	want := c.env.device.elicitations[s.as]
	f := c.awaitMessage(func(m map[string]any) bool { return m["method"] == "elicitation/complete" })
	if p, _ := f["params"].(map[string]any); p["elicitationId"] != want {
		t.Errorf("completed elicitation = %v, want %s", p["elicitationId"], want)
	}
}

// acpDeviceLogin checks whether the sign-in stored the login file of the store directory.
func acpDeviceLogin(want bool) acpCheck {
	return func(t *testing.T, c *acpConn) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(c.env.sessDir, "auth", "codex.json"))
		if got := err == nil; got != want {
			t.Errorf("login file stored = %v, want %v (%v)", got, want, err)
		}
		if want && !strings.Contains(string(raw), "access_token") {
			t.Errorf("login file = %s, want a login", raw)
		}
	}
}

// acpDeviceNoSecret fails when a frame holds a token or a code of the issuer.
func acpDeviceNoSecret(t *testing.T, c *acpConn) {
	t.Helper()
	var all strings.Builder
	for _, f := range c.wire {
		all.Write(f.Frame)
	}
	if holdsSecret(all.String(), c.env.device.issuer.Secrets()) {
		t.Error("a frame holds a token or a code")
	}
}

// acpDeviceCompletionFirst checks that the agent sent elicitation/complete before the answer of authenticate id.
func acpDeviceCompletionFirst(id string) acpCheck {
	return func(t *testing.T, c *acpConn) {
		t.Helper()
		complete, response := -1, -1
		for i, f := range c.wire {
			var m map[string]any
			if f.Dir != "agent" || json.Unmarshal(f.Frame, &m) != nil {
				continue
			}
			if m["method"] == "elicitation/complete" && complete < 0 {
				complete = i
			}
			if m["id"] == float64(c.pending[id]) && m["result"] != nil {
				response = i
			}
		}
		if complete < 0 || response < complete {
			t.Errorf("the completion is at frame %d and the authenticate answer at %d, want the completion first", complete, response)
		}
	}
}

// acpDeviceSlowedDown checks that the second poll came at least 5 s after the first.
func acpDeviceSlowedDown(t *testing.T, c *acpConn) {
	t.Helper()
	gaps := c.env.device.issuer.PollGaps()
	if len(gaps) == 0 || gaps[0] < 5*time.Second {
		t.Errorf("poll gaps = %v, want the first gap to be at least 5s after slow_down", gaps)
	}
}

var urlAuthInit = acpInit{version: 1, want: 1, elicitationURL: true, methods: []string{"chatgpt"}}

const declineCite = "`decline` ends the flow with `-32603`, \"The sign-in was declined\", and `cancel` ends it with `-32800`."

var acpDeviceScenarios = []acpScenario{
	{
		name:     "device_login_elicits_a_url_stores_the_login_and_completes",
		cite:     "When the user has entered the code, it stores the login, sends `elicitation/complete` with the `elicitationId`, and answers `{}`.",
		wasmOnly: true,
		device:   &deviceSpec{pending: 1},
		steps: []acpStep{
			urlAuthInit,
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpDeviceElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "accept"},
			acpAuthDone{id: "auth"},
			acpDeviceComplete{as: "login"},
			acpCheck(acpDeviceCompletionFirst("auth")),
			acpDeviceLogin(true),
			acpCheck(acpDeviceNoSecret),
		},
	},
	{
		name:     "device_login_waits_longer_after_slow_down",
		cite:     "When the issuer answers `slow_down`, the agent adds 5 seconds to the polling interval and goes on.",
		wasmOnly: true,
		device:   &deviceSpec{slowDown: 1},
		steps: []acpStep{
			urlAuthInit,
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpDeviceElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "accept"},
			acpAuthDone{id: "auth"},
			acpDeviceComplete{as: "login"},
			acpCheck(acpDeviceSlowedDown),
		},
	},
	{
		name:     "declined_device_login_elicitation_ends_the_sign_in",
		cite:     declineCite,
		wasmOnly: true,
		device:   &deviceSpec{pending: 1000},
		steps: []acpStep{
			urlAuthInit,
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpDeviceElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "decline"},
			acpAuthDone{id: "auth", code: acpInternal},
			acpDeviceLogin(false),
		},
	},
	{
		name:     "cancelled_device_login_elicitation_ends_the_sign_in",
		cite:     declineCite,
		wasmOnly: true,
		device:   &deviceSpec{pending: 1000},
		steps: []acpStep{
			urlAuthInit,
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpDeviceElicit{as: "login", auth: "auth"},
			acpAnswerElicitation{as: "login", action: "cancel"},
			acpAuthDone{id: "auth", code: acpRequestCanceled},
			acpDeviceLogin(false),
		},
	},
	{
		name:     "cancel_request_during_device_login_cancels_the_elicitation",
		cite:     "and the agent sends `$/cancel_request` for an elicitation that has no answer.",
		wasmOnly: true,
		device:   &deviceSpec{pending: 1000},
		steps: []acpStep{
			urlAuthInit,
			acpAuthStart{id: "auth", method: "chatgpt"},
			acpDeviceElicit{as: "login", auth: "auth"},
			acpCancelRequest{id: "auth"},
			acpAuthDone{id: "auth", code: acpRequestCanceled},
			acpAwaitCancelRequest{as: "login"},
			acpDeviceLogin(false),
		},
	},
	{
		name:     "second_authenticate_cancels_the_device_login_in_flight",
		cite:     "A new `authenticate` cancels the one in flight, whatever the method, and answers `-32800` to it.",
		wasmOnly: true,
		device:   &deviceSpec{pending: 1},
		steps: []acpStep{
			urlAuthInit,
			acpAuthStart{id: "first", method: "chatgpt"},
			acpDeviceElicit{as: "one", auth: "first"},
			acpAuthStart{id: "second", method: "chatgpt"},
			acpAuthDone{id: "first", code: acpRequestCanceled},
			acpAwaitCancelRequest{as: "one"},
			acpDeviceElicit{as: "two", auth: "second"},
			acpAnswerElicitation{as: "two", action: "accept"},
			acpAuthDone{id: "second"},
			acpDeviceComplete{as: "two"},
			acpDeviceLogin(true),
		},
	},
	{
		name:     "client_without_url_elicitation_gets_no_login",
		cite:     "Otherwise it offers no login, and `authMethods` is empty",
		wasmOnly: true,
		device:   &deviceSpec{},
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitation: true},
			acpCall{method: "authenticate", params: map[string]any{"methodId": "chatgpt"}, code: acpMethodNotFound},
			acpCall{method: "logout", params: map[string]any{}, code: acpMethodNotFound},
		},
	},
}
