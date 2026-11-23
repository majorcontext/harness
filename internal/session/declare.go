package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// maxDeclarationBytes bounds a declaration blob so that its sync batch fits protocol.MaxSyncBatchBytes.
const maxDeclarationBytes = 8 << 20

var errDeclarationTooLarge = errors.New("session: the prefix declaration is above the size bound")

// declaration is the system prompt, the tools, the deferred tool names, and the prompt segment that a session sends in
// front of its messages.
type declaration struct {
	Tools []protocol.ToolSpec `json:"tools"`
	// Deferred names the tools that were live and deferred when the declaration was built.
	Deferred []string `json:"deferred,omitempty"`
	Prompt   string   `json:"prompt,omitempty"`
	// Gone lists the MCP servers that had left the live set when the declaration was built.
	Gone []turn.Gone `json:"gone,omitempty"`
	// System is the system prompt segments that come before Prompt. nil: the blob was written before a declaration
	// held the system prompt.
	System []string `json:"system"`
	// Instructions names the instructions files of System, in order.
	Instructions []string `json:"instructions,omitempty"`
	// Skills are the skills that System lists.
	Skills []prompt.Skill `json:"skills,omitempty"`
	// Embedded are the embedded skills that System lists. Skill updates compare the newest list with it.
	Embedded []prompt.Embedded `json:"embedded,omitempty"`
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// withSystem returns d with the system prompt of info.
func (d declaration) withSystem(info prompt.Info) declaration {
	d.System = nonNil(slices.Clone(info.Segments))
	d.Instructions, d.Skills, d.Embedded = info.Instructions, info.Skills, info.Embedded
	return d
}

// info returns the system prompt of d.
func (d declaration) info() prompt.Info {
	return prompt.Info{Segments: d.System, Instructions: d.Instructions, Skills: d.Skills, Embedded: d.Embedded}
}

// encode returns the blob bytes and the key "prefix-<sha256 hex of the bytes>".
func (d declaration) encode() (data []byte, key string, err error) {
	if d.Tools == nil {
		d.Tools = []protocol.ToolSpec{}
	}
	if d.System == nil {
		d.System = []string{}
	}
	if data, err = json.Marshal(d); err != nil {
		return nil, "", err
	}
	if len(data) > maxDeclarationBytes {
		return nil, "", errDeclarationTooLarge
	}
	sum := sha256.Sum256(data)
	return data, "prefix-" + hex.EncodeToString(sum[:]), nil
}

func decodeDeclaration(data []byte) (declaration, error) {
	var d declaration
	if err := json.Unmarshal(data, &d); err != nil {
		return declaration{}, err
	}
	if d.Tools == nil {
		d.Tools = []protocol.ToolSpec{}
	}
	return d, nil
}

// schemaChanged reports whether a tool of d has another input schema in live.
func (d declaration) schemaChanged(live []protocol.ToolSpec) bool {
	for _, t := range d.Tools {
		for _, l := range live {
			if l.Name == t.Name && compactJSON(l.InputSchema) != compactJSON(t.InputSchema) {
				return true
			}
		}
	}
	return false
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// decodedDeclaration is the declaration of the blob key.
type decodedDeclaration struct {
	key string
	d   declaration
}

// declared wraps the Source of a session and maps each Toolset through the newest declaration.
type declared struct {
	a   *Actor
	src turn.Source
}

// Toolset implements turn.Source. A backend that owns the loop gets the live Toolset. Any other backend gets the
// declared tools and prompt segment, with each live tool that the declaration lacks deferred.
func (d declared) Toolset(ctx context.Context, history []protocol.LogMessage, allowed []string, model string) turn.Toolset {
	ts := d.src.Toolset(ctx, history, allowed, model)
	warm := turn.IsWarm(ctx)
	if d.a.cfg.Backend.Capabilities(model).OwnsLoop {
		return d.withSystem(ts, d.a.livePrompt(ctx), len(history), warm)
	}
	live := declaration{Tools: []protocol.ToolSpec{}, Prompt: ts.Prompt, Gone: ts.Gone}
	for _, t := range ts.Tools {
		live.Tools = append(live.Tools, t.Spec())
	}
	for _, t := range ts.Deferred {
		if _, ok := t.(turn.Declarable); !ok {
			live.Deferred = append(live.Deferred, t.Spec().Name)
		}
	}
	decl, search, ok := d.a.declare(ctx, live, model, warm)
	if !ok {
		return d.withSystem(ts, d.a.livePrompt(ctx), len(history), warm)
	}
	ts = d.withSystem(ts, decl.info(), len(history), warm)
	ts.Specs, ts.Prompt, ts.SearchAllowed = decl.Tools, decl.Prompt, &search
	has := func(name string) bool {
		return slices.ContainsFunc(decl.Tools, func(s protocol.ToolSpec) bool { return s.Name == name })
	}
	accounts := func(name string) bool { return has(name) || slices.Contains(decl.Deferred, name) }
	ts.Deferred, ts.Tools = slices.Clip(slices.Clone(ts.Deferred)), slices.Clone(ts.Tools)
	for i, t := range ts.Deferred {
		if dt, ok := t.(turn.Declarable); ok {
			var notices []turn.Notice
			ts.Deferred[i], notices = dt.Declared(has, accounts, decl.Prompt, decl.Gone)
			ts.Notices = replaceNotices(ts.Notices, notices)
		}
	}
	for i, t := range ts.Tools {
		if !has(live.Tools[i].Name) {
			ts.Deferred = append(ts.Deferred, t)
		}
		if dt, ok := t.(turn.Declarable); ok {
			var notices []turn.Notice
			ts.Tools[i], notices = dt.Declared(has, accounts, decl.Prompt, decl.Gone)
			ts.Notices = replaceNotices(ts.Notices, notices)
		}
	}
	return ts
}

// withSystem sets the system prompt info on ts, and the skills notice that compares the newest list with the list of info.
func (d declared) withSystem(ts turn.Toolset, info prompt.Info, at int, warm bool) turn.Toolset {
	ts.System, ts.SkillList = nonNil(info.Segments), info.SkillList
	if d.a.cfg.SkillsNotice != nil {
		if n, ok := d.a.cfg.SkillsNotice(info.Embedded, at); ok {
			ts.Notices = replaceNotices(ts.Notices, []turn.Notice{n})
		}
	}
	if !warm {
		d.a.usePrompt(info)
	}
	return ts
}

// replaceNotices returns notices with each notice of repl in place of the notice of its Kind.
func replaceNotices(notices, repl []turn.Notice) []turn.Notice {
	out := slices.Clone(notices)
	for _, r := range repl {
		if i := slices.IndexFunc(out, func(n turn.Notice) bool { return n.Kind == r.Kind }); i >= 0 {
			out[i] = r
		} else {
			out = append(out, r)
		}
	}
	return out
}

// declView is what the actor goroutine knows about the newest declaration.
type declView struct {
	rec    protocol.PrefixDeclared
	has    bool
	due    bool
	cached *declaration
	first  bool
}

func (a *Actor) declView() declView {
	v := declView{first: !a.declChecked, due: a.state.DeclarationDue()}
	if v.rec, v.has = a.state.Declared(); v.has && a.decl.key == v.rec.BlobKey {
		v.cached = &a.decl.d
	}
	return v
}

func (a *Actor) searchAllowed() bool {
	return a.cfg.WebSearch && protocol.ToolAllowed(a.state.AllowedTools(), turn.WebSearchTool)
}

// declare returns the declaration that a model call sends, or that a warm-up reads when readOnly, which appends no record, the search_allowed of its record, and false when the call
// sends the live tools. The check of the input schemas stays pending until a call ends with no failed write. It
// appends prefix.declared when the log holds none, after a compaction between turns, at a change of the model
// with other tools, and at the first call of a process when a declared tool has another input schema. A blob that
// the store lacks or that does not decode is declared again from live.
func (a *Actor) declare(ctx context.Context, live declaration, model string, readOnly bool) (declaration, bool, bool) {
	a.declMu.Lock()
	defer a.declMu.Unlock()
	v, err := call(ctx, a, func(reply func(declView, error)) { reply(a.declView(), nil) })
	if err != nil {
		return declaration{}, false, false
	}
	v.first = v.first && !readOnly
	missing := false
	if v.has && v.cached == nil {
		d, err := a.readDeclaration(ctx, v.rec.BlobKey)
		if ctx.Err() != nil {
			return declaration{}, false, false
		}
		if err != nil {
			a.log().Warn("harness: the prefix declaration cannot be read; declaring again from live state", "session", a.cfg.ID, "blob", v.rec.BlobKey, "err", err)
			missing = true
		} else {
			v.cached = &d
			a.setDecl(ctx, v.rec.BlobKey, d)
		}
	}
	current, usable := declaration{}, v.has && !missing
	if usable {
		current = *v.cached
		usable = current.System != nil
	}
	forced := !usable || v.due
	if !forced && v.rec.Model == model && (!v.first || !current.schemaChanged(live.Tools)) {
		a.markChecked(ctx, v.first)
		return current, v.rec.SearchAllowed, true
	}
	if readOnly {
		return current, v.rec.SearchAllowed, usable
	}
	if !forced {
		unchanged := live.withSystem(current.info())
		unchanged.Gone = current.Gone
		_, same, err := unchanged.encode()
		if err == nil && same == v.rec.BlobKey {
			a.markChecked(ctx, v.first)
			return current, v.rec.SearchAllowed, true
		}
	}
	info := a.readPrompt(ctx)
	if ctx.Err() != nil {
		return current, v.rec.SearchAllowed, usable
	}
	live = live.withSystem(info)
	data, key, err := live.encode()
	if err != nil {
		if !a.oversized {
			a.oversized = true
			a.log().Error("harness: the prefix declaration cannot be recorded; the session sends live tools", "session", a.cfg.ID, "err", err)
		}
		return declaration{}, false, false
	}
	a.oversized = false
	if err := a.cfg.Store.PutBlob(ctx, key, bytes.NewReader(data)); err != nil {
		a.log().Warn("harness: the prefix declaration blob cannot be written", "session", a.cfg.ID, "err", err)
		return current, v.rec.SearchAllowed, usable
	}
	search := false
	_, err = call(ctx, a, func(reply func(struct{}, error)) {
		search = a.searchAllowed()
		err := a.append(protocol.PrefixDeclared{BlobKey: key, Bytes: len(data), Model: model, SearchAllowed: search})
		if err == nil {
			a.decl, a.declChecked = decodedDeclaration{key, live}, true
		}
		reply(struct{}{}, err)
	})
	if err != nil {
		a.log().Warn("harness: the prefix declaration cannot be appended", "session", a.cfg.ID, "err", err)
		return current, v.rec.SearchAllowed, usable
	}
	return live, search, true
}

func (a *Actor) markChecked(ctx context.Context, first bool) {
	if first {
		_, _ = call(ctx, a, func(reply func(struct{}, error)) {
			a.declChecked = true
			reply(struct{}{}, nil)
		})
	}
}

func (a *Actor) readDeclaration(ctx context.Context, key string) (declaration, error) {
	data, err := readBlob(ctx, a.cfg.Store, key)
	if err != nil {
		return declaration{}, err
	}
	return decodeDeclaration(data)
}

func (a *Actor) setDecl(ctx context.Context, key string, d declaration) {
	_, _ = call(ctx, a, func(reply func(struct{}, error)) {
		a.decl = decodedDeclaration{key, d}
		reply(struct{}{}, nil)
	})
}
