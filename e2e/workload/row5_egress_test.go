//go:build e2e

package workload

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Test_Row5_EgressNAT is acceptance row 5: egress from a box to bifrost, to
// GitHub, and to npm all goes out through Cloud NAT static IPs, and the
// observed source IP is one of those static IPs (not, say, a pod IP or an
// unexpected route).
//
// It drives a real box's bash tool (there is no lower-level exec surface
// on either API this suite has a spec for — see doc.go) to curl an IP-echo
// endpoint and each real destination, then compares the echoed source IP
// against the configured NAT range. Two env vars gate what actually gets
// asserted:
//
//   - EGRESS_IP_ECHO_URL: an HTTPS endpoint that echoes the caller's source
//     IP as its whole response body (default https://api.ipify.org).
//   - NAT_STATIC_IPS: comma-separated static IPs Cloud NAT is configured to
//     use. Without it, this test still exercises reachability to bifrost/
//     GitHub/npm but SKIPS the NAT-IP-match assertion — BLOCKED-ON-
//     infra-provided-NAT-IP-list, since that list is provisioned by the
//     infra/deploy side this suite does not own.
//   - BIFROST_URL: bifrost's own reachable URL (default
//     https://gatekeeper.majorcontext.dev, see docs/deploy-modal.md's
//     gatekeeper reference) — override if the deployed bifrost endpoint
//     differs.
func Test_Row5_EgressNAT(t *testing.T) {
	client, ok, missing := NewBoxesClient()
	if !ok {
		t.Skip(skipReason("boxes service reachable", "missing "+missing))
	}

	ctx, cancel := context.WithTimeout(context.Background(), spawnPollDeadline+5*time.Minute)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Skip(skipReason("boxes service reachable", err.Error()))
	}

	ref := imageRef()
	name := fmt.Sprintf("e2e-row5-%d", time.Now().UnixNano())
	box, err := client.Spawn(ctx, SpawnRequest{Name: name, Image: ref})
	if err != nil {
		t.Skip(skipReason("box spawn accepted", err.Error()))
	}
	t.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer delCancel()
		_ = client.Delete(delCtx, box.ID)
	})

	running := waitForRunningBox(t, ctx, client, box.ID)
	hc := NewHarnessClient(running.TunnelURL, running.RunToken)

	sess, err := hc.CreateSession(ctx, CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		endCtx, endCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer endCancel()
		_ = hc.EndSession(endCtx, sess.ID)
	})

	echoURL := envOrDefault("EGRESS_IP_ECHO_URL", "https://api.ipify.org")
	bifrostURL := envOrDefault("BIFROST_URL", "https://gatekeeper.majorcontext.dev")

	directive := fmt.Sprintf(
		"Using the bash tool, run these curl commands one at a time and reply with "+
			"ONLY their combined output, one line per command, in this order: "+
			"(1) `curl -sS --max-time 10 %s` (2) `curl -sS -o /dev/null -w '%%{http_code}' --max-time 10 %s` "+
			"(3) `curl -sS -o /dev/null -w '%%{http_code}' --max-time 10 https://github.com` "+
			"(4) `curl -sS -o /dev/null -w '%%{http_code}' --max-time 10 https://registry.npmjs.org`",
		echoURL, bifrostURL,
	)
	resp, err := hc.PromptAsync(ctx, sess.ID, directive)
	if err != nil {
		t.Fatalf("prompt egress checks: %v", err)
	}
	outcome, err := waitForTurnEnd(ctx, hc, sess.ID, resp.Seq-1)
	if err != nil {
		t.Fatalf("waiting for egress-check turn: %v", err)
	}
	if outcome.Outcome != "completed" {
		t.Fatalf("egress-check turn ended with outcome=%s error=%s", outcome.Outcome, outcome.Error)
	}

	transcript, err := hc.GetMessages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get transcript: %v", err)
	}
	reply := strings.TrimSpace(LastAssistantText(transcript))
	lines := strings.Split(reply, "\n")
	if len(lines) < 4 {
		t.Fatalf("expected 4 lines of curl output (echo IP, bifrost, github, npm), got %d: %q", len(lines), reply)
	}
	observedIP := strings.TrimSpace(lines[0])
	t.Logf("egress checks from box %s: source_ip=%s bifrost=%s github=%s npm=%s",
		box.ID, observedIP, lines[1], lines[2], lines[3])
	for i, dest := range []string{"bifrost", "github", "npm"} {
		if code := strings.TrimSpace(lines[i+1]); code == "" || code == "000" {
			t.Errorf("egress to %s did not complete (http_code=%q)", dest, code)
		}
	}

	natIPs := os.Getenv("NAT_STATIC_IPS")
	if natIPs == "" {
		t.Log(skipReason("NAT static IP list", "NAT_STATIC_IPS not set; verified reachability only, not source-IP match"))
		return
	}
	want := strings.Split(natIPs, ",")
	matched := false
	for _, ip := range want {
		if strings.TrimSpace(ip) == observedIP {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("observed egress source IP %s is not in the configured Cloud NAT static IP set %v", observedIP, want)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
