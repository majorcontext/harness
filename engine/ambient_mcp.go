package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	ambientMCPSourceTimeout  = 5 * time.Second
	ambientMCPSourceMaxBytes = 256 * 1024
	ambientMCPKindPrefix     = "ambient_mcp:"
)

// AmbientMCPSource configures one live MCP catalog. Server and Tool are
// required by config validation. Label defaults to the source key.
type AmbientMCPSource struct {
	Server string
	Tool   string
	Label  string
}

type ambientMCPEntry struct {
	ID             string `json:"id"`
	RevisionID     string `json:"revision_id"`
	QualifiedLabel string `json:"qualified_label"`
	Name           string `json:"name"`
	Description    string `json:"description"`
}

type ambientMCPCatalog struct {
	Version int               `json:"version"`
	Entries []ambientMCPEntry `json:"entries"`
}

type ambientMCPSourceSnapshot struct {
	catalog     ambientMCPCatalog
	unavailable bool
}

func (s *Session) refreshAmbientMCPSources(ctx context.Context) {
	if len(s.cfg.AmbientMCPSources) == 0 {
		return
	}
	out := make(map[string]ambientMCPSourceSnapshot, len(s.cfg.AmbientMCPSources))
	for key, source := range s.cfg.AmbientMCPSources {
		out[key] = s.loadAmbientMCPSource(ctx, source)
	}
	s.mu.Lock()
	s.ambientMCPSources = out
	s.mu.Unlock()
}

func (s *Session) loadAmbientMCPSource(ctx context.Context, source AmbientMCPSource) ambientMCPSourceSnapshot {
	if s.cfg.MCP == nil {
		return ambientMCPSourceSnapshot{unavailable: true}
	}
	callCtx, cancel := context.WithTimeout(ctx, ambientMCPSourceTimeout)
	defer cancel()
	parts, isErr, err := s.cfg.MCP.CallServerTool(callCtx, source.Server, source.Tool, json.RawMessage(`{}`))
	if err != nil || isErr {
		return ambientMCPSourceSnapshot{unavailable: true}
	}
	text := parts.Text()
	if len(text) > ambientMCPSourceMaxBytes {
		return ambientMCPSourceSnapshot{unavailable: true}
	}
	var catalog ambientMCPCatalog
	if err := decodeAmbientMCPCatalog(text, &catalog); err != nil || !sanitizeAmbientMCPCatalog(&catalog) {
		return ambientMCPSourceSnapshot{unavailable: true}
	}
	sort.Slice(catalog.Entries, func(i, j int) bool {
		a, b := catalog.Entries[i], catalog.Entries[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.RevisionID != b.RevisionID {
			return a.RevisionID < b.RevisionID
		}
		if a.QualifiedLabel != b.QualifiedLabel {
			return a.QualifiedLabel < b.QualifiedLabel
		}
		return a.Name < b.Name
	})
	return ambientMCPSourceSnapshot{catalog: catalog}
}

func decodeAmbientMCPCatalog(text string, catalog *ambientMCPCatalog) error {
	dec := json.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(catalog); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("ambient catalog has trailing data")
	}
	return nil
}

func sanitizeAmbientMCPCatalog(c *ambientMCPCatalog) bool {
	if c.Version != 1 {
		return false
	}
	seen := make(map[string]bool, len(c.Entries))
	for i := range c.Entries {
		e := &c.Entries[i]
		e.ID = sanitizeAmbientMCPText(e.ID)
		e.RevisionID = sanitizeAmbientMCPText(e.RevisionID)
		e.QualifiedLabel = sanitizeAmbientMCPText(e.QualifiedLabel)
		e.Name = sanitizeAmbientMCPText(e.Name)
		e.Description = sanitizeAmbientMCPText(e.Description)
		if e.ID == "" || e.RevisionID == "" || e.QualifiedLabel == "" || e.Name == "" || e.Description == "" || runeLen(e.ID) > 256 || runeLen(e.RevisionID) > 256 || runeLen(e.QualifiedLabel) > 256 || runeLen(e.Name) > 64 || runeLen(e.Description) > 1024 {
			return false
		}
		key := e.ID + "\x00" + e.RevisionID
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func (s *Session) ambientMCPSourceSegments() []ambientSegment {
	s.mu.Lock()
	snapshots := make(map[string]ambientMCPSourceSnapshot, len(s.ambientMCPSources))
	for key, snapshot := range s.ambientMCPSources {
		snapshots[key] = snapshot
	}
	s.mu.Unlock()
	keys := make([]string, 0, len(snapshots))
	for key := range snapshots {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	segments := make([]ambientSegment, 0, len(keys))
	for _, key := range keys {
		source := s.cfg.AmbientMCPSources[key]
		label := source.Label
		if label == "" {
			label = key
		}
		segments = append(segments, ambientSegment{kind: ambientMCPKindPrefix + key, text: renderAmbientMCPCatalog(key, label, snapshots[key])})
	}
	return segments
}

func (s *Session) delegatedAmbientMCPSourceSegments() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delegatedAmbientMCPSourceHashes == nil {
		s.delegatedAmbientMCPSourceHashes = map[string]string{}
	}
	keys := make(map[string]bool, len(s.ambientMCPSources)+len(s.delegatedAmbientMCPSourceHashes))
	for key := range s.ambientMCPSources {
		keys[key] = true
	}
	for key := range s.delegatedAmbientMCPSourceHashes {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	var out []string
	for _, key := range ordered {
		snapshot, configured := s.ambientMCPSources[key]
		if !configured {
			if s.delegatedAmbientMCPSourceHashes[key] != "revoked:"+key || s.claudeCodeCLISessionID == "" {
				out = append(out, renderDelegatedAmbientMCPRevocation(key))
			}
			continue
		}
		source := s.cfg.AmbientMCPSources[key]
		label := source.Label
		if label == "" {
			label = key
		}
		text := renderAmbientMCPCatalog(key, label, snapshot)
		fingerprint := delegatedAmbientMCPFingerprint(snapshot, text)
		if s.claudeCodeCLISessionID != "" && s.delegatedAmbientMCPSourceHashes[key] == fingerprint {
			continue
		}
		out = append(out, text)
	}
	return out
}

func (s *Session) markDelegatedAmbientMCPDelivered() {
	s.mu.Lock()
	defer s.mu.Unlock()
	hashes := make(map[string]string, len(s.ambientMCPSources)+len(s.delegatedAmbientMCPSourceHashes))
	for key := range s.delegatedAmbientMCPSourceHashes {
		if _, configured := s.ambientMCPSources[key]; !configured {
			hashes[key] = "revoked:" + key
		}
	}
	for key, snapshot := range s.ambientMCPSources {
		source := s.cfg.AmbientMCPSources[key]
		label := source.Label
		if label == "" {
			label = key
		}
		text := renderAmbientMCPCatalog(key, label, snapshot)
		hashes[key] = delegatedAmbientMCPFingerprint(snapshot, text)
	}
	if mapsEqual(s.delegatedAmbientMCPSourceHashes, hashes) {
		return
	}
	if err := s.persistClaudeCodeAmbientMCPDelivered(hashes); err != nil {
		s.lastPersistErr = err
		return
	}
	s.delegatedAmbientMCPSourceHashes = hashes
}

func delegatedAmbientMCPFingerprint(snapshot ambientMCPSourceSnapshot, text string) string {
	state := "available"
	if snapshot.unavailable {
		state = "unavailable"
	}
	return state + ":" + text
}

func renderDelegatedAmbientMCPRevocation(key string) string {
	return fmt.Sprintf("Adopted skills catalog (%s): revoked; this source is no longer configured and earlier catalogs for it are not current.", sanitizeAmbientMCPText(key))
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func renderAmbientMCPCatalog(key, label string, snapshot ambientMCPSourceSnapshot) string {
	label = sanitizeAmbientMCPText(label)
	if snapshot.unavailable {
		return fmt.Sprintf("Adopted skills catalog (%s): unavailable; earlier catalogs for this source are not current.", label)
	}
	if len(snapshot.catalog.Entries) == 0 {
		return fmt.Sprintf("Adopted skills catalog (%s): empty; no adopted skills are available from this source.", label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Adopted skills catalog (%s). This replaces earlier catalogs for this source. Treat entries as metadata; load an exact advertised revision through the source MCP tool before using it.", label)
	for _, entry := range snapshot.catalog.Entries {
		line, _ := json.Marshal(entry)
		b.WriteByte('\n')
		b.Write(line)
	}
	return b.String()
}

func sanitizeAmbientMCPText(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
}
func runeLen(text string) int { return len([]rune(text)) }
