package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// prefix break kinds that a scenario can declare in prefixBreaks.
const (
	breakTools    = "tools"
	breakSystem   = "system"
	breakCacheKey = "cache_key"
	breakHistory  = "history"
)

// notTurnMarkers are texts of the system prompt of a request that is no turn of a session: a title, a summary, and a goal evaluator each have a stream of their own.
var notTurnMarkers = []string{"Generate a short title for a conversation", "You are summarizing a prefix", goalEvaluatorMarker}

// checkPrefixes checks that each request of a session after the first sends the tools, the system prompt, and the cache key of the request before it, and a history that starts with the bytes of that request's history. Only the cache_control marker may move. A break that the spec designs is declared in breaks with its reason; a declared break that never happens fails.
func checkPrefixes(t *testing.T, raws [][]byte, breaks map[string]string) {
	t.Helper()
	type wireReq struct {
		fields map[string]string
		hist   []string
		incr   bool
	}
	streams := map[string][]wireReq{}
	var order, restarts []string
	for _, raw := range raws {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("model request is not JSON: %v", err)
		}
		key, ok := sessionStream(body)
		if !ok {
			continue
		}
		req := wireReq{fields: map[string]string{}, incr: len(body["previous_response_id"]) > 0 && string(body["previous_response_id"]) != `""`}
		for _, f := range []string{"tools", "system", "instructions", "prompt_cache_key"} {
			req.fields[f] = cacheControl.ReplaceAllString(string(body[f]), "")
		}
		for _, f := range []string{"messages", "input"} {
			var items []json.RawMessage
			if json.Unmarshal(body[f], &items) == nil {
				for _, it := range items {
					req.hist = append(req.hist, historyBlocks(cacheControl.ReplaceAllString(string(it), ""))...)
				}
			}
		}
		if _, seen := streams[key]; !seen {
			order = append(order, key)
			if strings.HasPrefix(key, anthropicStreamPrefix) && hasAssistant(body["messages"]) {
				restarts = append(restarts, key)
			}
		}
		streams[key] = append(streams[key], req)
	}
	used := map[string]bool{}
	broke := func(kind, stream string, n int, detail string) {
		if _, ok := breaks[kind]; ok {
			used[kind] = true
			return
		}
		t.Errorf("session %s: request %d breaks the cached prefix (%s): %s", stream, n, kind, detail)
	}
	for _, stream := range restarts {
		broke(breakHistory, stream, 1, "the first request of the stream holds an assistant message, so the first user text moved or the history restarted")
	}
	for _, stream := range order {
		reqs := streams[stream]
		for i := 1; i < len(reqs); i++ {
			prev, cur := reqs[i-1], reqs[i]
			for _, f := range []struct{ kind, field string }{{breakTools, "tools"}, {breakSystem, "system"}, {breakSystem, "instructions"}, {breakCacheKey, "prompt_cache_key"}} {
				if cur.fields[f.field] != prev.fields[f.field] {
					broke(f.kind, stream, i+1, f.field+" "+firstDiff(prev.fields[f.field], cur.fields[f.field]))
				}
			}
			if cur.incr || prev.incr {
				continue
			}
			if n := len(prev.hist); n > 0 && (len(cur.hist) < n || !slices.Equal(cur.hist[:n], prev.hist)) {
				broke(breakHistory, stream, i+1, "history "+firstDiff(strings.Join(prev.hist, ","), strings.Join(cur.hist, ",")))
			}
		}
	}
	var unused []string
	for kind, reason := range breaks {
		if !used[kind] {
			unused = append(unused, fmt.Sprintf("%s (%s)", kind, reason))
		}
	}
	sort.Strings(unused)
	for _, u := range unused {
		t.Errorf("declared prefix break never happened: %s", u)
	}
}

const anthropicStreamPrefix = "first user text "

// hasAssistant reports whether messages holds a message of the assistant. The first request of a session holds user content only.
func hasAssistant(messages json.RawMessage) bool {
	var msgs []struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(messages, &msgs)
	return slices.ContainsFunc(msgs, func(m struct {
		Role string `json:"role"`
	}) bool {
		return m.Role == "assistant"
	})
}

// sessionStream names the session of a request, or reports false for a request that is no turn. The Anthropic wire carries no session key, so its first user text names it.
func sessionStream(body map[string]json.RawMessage) (string, bool) {
	var head string
	for _, f := range []string{"system", "instructions"} {
		head += string(body[f])
	}
	var msgs []json.RawMessage
	_ = json.Unmarshal(body["messages"], &msgs)
	if len(msgs) > 0 {
		head += string(msgs[0])
	}
	head = strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(head)
	for _, m := range notTurnMarkers {
		if strings.Contains(head, m) {
			return "", false
		}
	}
	for _, f := range []string{"user", "prompt_cache_key"} {
		var s string
		if json.Unmarshal(body[f], &s) == nil && s != "" {
			return s, true
		}
	}
	if len(msgs) == 0 {
		return "", false
	}
	var first struct {
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(msgs[0], &first)
	var blocks []struct {
		Text string `json:"text"`
	}
	var text string
	if json.Unmarshal(first.Content, &text) != nil && json.Unmarshal(first.Content, &blocks) == nil && len(blocks) > 0 {
		text = blocks[0].Text
	}
	return anthropicStreamPrefix + fmt.Sprintf("%q", text), true
}

// historyBlocks splits an Anthropic message into its content blocks, because the wire merges neighbor messages of one role and a provider caches by block. Any other item is one entry.
func historyBlocks(item string) []string {
	var m struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal([]byte(item), &m) != nil || m.Role == "" || len(m.Content) == 0 {
		return []string{item}
	}
	out := make([]string, len(m.Content))
	for i, b := range m.Content {
		out[i] = m.Role + "|" + string(b)
	}
	return out
}

// firstDiff shows the bytes of a and b around the first byte that differs.
func firstDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	around := func(s string) string { return s[max(0, i-30):min(len(s), i+60)] }
	return fmt.Sprintf("differs at byte %d: before %q, after %q", i, around(a), around(b))
}

// prefixBreak declares the break kinds of a scenario with one reason.
func prefixBreak(reason string, kinds ...string) map[string]string {
	out := map[string]string{}
	for _, k := range kinds {
		out[k] = reason
	}
	return out
}

func compactionBreak() map[string]string {
	return prefixBreak("a compaction replaces the folded history with its summary", breakHistory)
}

func foreignHistoryBreak() map[string]string {
	return prefixBreak("the first request to this model replays history that another backend or model produced", breakHistory)
}
