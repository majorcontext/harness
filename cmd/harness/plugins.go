// Plugin wiring for cmd/harness: reading config.Plugins, resolving cached
// manifests (probing only when the cache misses), constructing the
// *plugin.Host that both run and serve pass through as engine.Config.Hooks,
// and the `harness plugin probe` subcommand. Package plugin itself (Host,
// Probe, chain dispatch) is a complete, separately tested library — nothing
// here reimplements its lazy-spawn or chaining semantics; this file only
// resolves manifests and wires the result in.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/plugin"
)

// pluginProbeTimeout bounds `harness plugin probe` and the as-needed probing
// buildPluginHost performs when a configured plugin's binary hash is not yet
// cached. It is not the per-hook dispatch deadline (that is
// plugin.Options.HookTimeout, inside plugin.Host itself) — this only bounds
// the one-time initialize handshake a fresh probe performs.
const pluginProbeTimeout = 30 * time.Second

// pluginCachePath resolves the on-disk manifest cache file: $HARNESS_PLUGIN_CACHE
// if set, otherwise a file next to the user config (see config.Path), so a
// machine with a custom $HARNESS_CONFIG also gets a private plugin cache.
func pluginCachePath() string {
	if p := os.Getenv("HARNESS_PLUGIN_CACHE"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(config.Path()), "plugin_cache.json")
}

// pluginManifestCache is the on-disk probe cache described in
// plugin/AGENTS.md. Entries are keyed by plugin name *and* a digest of the
// plugin's Config/Env/Dir (see pluginCacheKey/pluginSpecDigest) so a
// renamed config entry, or a plugin whose config/env/dir changed since it
// was last probed, both re-probe rather than silently reusing an unrelated
// or stale manifest. The binary's own identity (content hash, size, mtime)
// lives inside each entry (pluginCacheEntry), not the key, so it can be
// checked cheaply (see pluginBinaryIdentity) without hashing on every
// startup.
type pluginManifestCache struct {
	Entries map[string]pluginCacheEntry `json:"entries"`
}

// pluginCacheEntry is one manifest-cache entry: the probed manifest plus
// enough about the binary that produced it to detect staleness without
// paying to hash it on every startup. BinaryHash (sha256 of the executable
// content) is the ground truth for "did the binary change" — it is what
// pluginCacheKey's sibling, the spec digest, doesn't cover. Size and ModTime
// are a fast, hash-free proxy for the same question: see
// pluginBinaryIdentity's comment for the mtime-granularity tradeoff that
// buys.
type pluginCacheEntry struct {
	BinaryHash string `json:"binary_hash"`
	Size       int64  `json:"size"`
	ModTimeNS  int64  `json:"mtime_unix_nano"`
	// ScriptFiles and ScriptHash extend the same staleness check to an
	// interpreter-wrapped plugin's script argument (see pluginScriptFiles):
	// a plain plugin (command == [binary]) has none, so both are omitted
	// and an old on-disk cache entry from before this field existed decodes
	// as the same "no script files" case — unchanged behavior for finding
	// (d).
	ScriptFiles []pluginFileStat `json:"script_files,omitempty"`
	ScriptHash  string           `json:"script_hash,omitempty"`
	Manifest    plugin.Manifest  `json:"manifest"`
}

// pluginFileStat is the (path, size, mtime) identity of one script argument
// resolved by pluginScriptFiles — the same (size, mtime)-then-hash shape
// pluginBinaryIdentity/pluginBinaryHashAt already use for command[0], not a
// second scheme.
type pluginFileStat struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	ModTimeNS int64  `json:"mtime_unix_nano"`
}

// loadPluginManifestCache reads the on-disk manifest cache. A missing file
// is an empty cache (nothing has ever been probed). A present-but-corrupt
// file — most commonly a concurrent reader catching an in-progress write
// under the pre-atomic-rename save(), but any other corruption too — is
// treated the same as a cache miss: every plugin simply re-probes and the
// next save() overwrites the bad file. It is never a startup failure; only
// an I/O error opening the file (permissions, etc.) is.
func loadPluginManifestCache(path string) (*pluginManifestCache, error) {
	c := &pluginManifestCache{Entries: map[string]pluginCacheEntry{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(c); err != nil {
		fmt.Fprintf(os.Stderr, "plugin cache: ignoring corrupt cache file %s (re-probing): %v\n", path, err)
		return &pluginManifestCache{Entries: map[string]pluginCacheEntry{}}, nil
	}
	if c.Entries == nil {
		c.Entries = map[string]pluginCacheEntry{}
	}
	return c, nil
}

// save persists the cache atomically: it writes the full content to a temp
// file in the same directory (so the rename below is on the same
// filesystem) and renames it over path. A rename is atomic from any
// concurrent reader's point of view — it either sees the old file or the
// complete new one, never a half-written one — unlike a truncating
// os.WriteFile, which a concurrent loadPluginManifestCache could catch
// mid-write and (pre-fix) fail startup on.
func (c *pluginManifestCache) save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".plugin_cache-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// pluginCacheKey identifies one cache entry: the config name (so chaining
// order and identity are unambiguous) plus specDigest, a digest of the
// plugin's Config/Env/Dir (see pluginSpecDigest). The binary's own identity
// deliberately is *not* folded into the key — it lives inside the entry
// (pluginCacheEntry) instead, so it can be checked via a cheap stat rather
// than forcing every lookup to hash the file first (see
// pluginBinaryIdentity).
func pluginCacheKey(name, specDigest string) string { return name + "@" + specDigest }

// pluginSpecDigest returns a stable digest of the parts of a plugin's
// identity that a binary-content hash never covers: Config, Env, and Dir.
// Folding it into the cache key means changing any of these in config.json
// — without rebuilding the plugin binary — is a cache miss and triggers a
// re-probe, rather than silently serving a manifest cached for a
// differently-configured process. This is the same divergence the
// ProbeSpec fix (see buildPluginSpecs) closed for probing itself; this
// closes it for the cache key too.
//
// cfg is decoded into an untyped value before being folded into the
// digested struct so insignificant JSON differences in config.json (key
// order, whitespace) don't cause spurious cache misses: json.Marshal of a
// map[string]any sorts keys, so two byte-different-but-semantically-equal
// RawMessages produce the same digest.
func pluginSpecDigest(cfg json.RawMessage, env []string, dir string) (string, error) {
	var cfgVal any
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &cfgVal); err != nil {
			return "", fmt.Errorf("plugin config: %w", err)
		}
	}
	b, err := json.Marshal(struct {
		Config any      `json:"config"`
		Env    []string `json:"env"`
		Dir    string   `json:"dir"`
	}{Config: cfgVal, Env: env, Dir: dir})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// pluginBinaryHashCalls counts every time pluginBinaryHash actually reads a
// plugin executable's content to hash it. Production code never reads it;
// it exists so tests can prove the stat-based fast path in buildPluginSpecs
// avoids re-hashing an unchanged binary on a cache hit, without reaching
// into unexported state any other way.
var pluginBinaryHashCalls atomic.Int64

// pluginBinaryHash hashes the plugin's executable so a changed binary
// invalidates its cache entry. command[0] is resolved via
// plugin.ResolveExecutable — the exact same rules (and the exact same
// function) the real spawn path (plugin.Host's instance.dial) uses, keyed
// off the same dir — so the hash can never track a different file than the
// one that executes.
//
// This hashes the entire binary, which is the correct ground truth but not
// free to do on every startup — see pluginBinaryIdentity, which is what
// buildPluginSpecs actually calls on the common path, falling back to this
// only when a stat-based fast path can't rule out a change.
func pluginBinaryHash(command []string, dir string) (string, error) {
	path, err := plugin.ResolveExecutable(command, dir)
	if err != nil {
		return "", err
	}
	return pluginBinaryHashAt(path)
}

// pluginBinaryHashAt hashes the file at an already-resolved executable
// path, without re-resolving command[0]. Split out of pluginBinaryHash so
// buildPluginSpecs's fast/slow path can share one os.Stat/ResolveExecutable
// call instead of resolving the executable twice.
func pluginBinaryHashAt(path string) (string, error) {
	pluginBinaryHashCalls.Add(1)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pluginBinaryIdentity stats the plugin's resolved executable and reports
// its size and modification time (as UnixNano, since os.FileInfo.ModTime's
// precision is itself filesystem/OS dependent — nanoseconds is simply the
// widest representation Go exposes, not a promise of nanosecond
// resolution). buildPluginSpecs uses (size, mtime) as a cheap proxy for "is
// this the same binary I already hashed and probed": most filesystems in
// practice have mtime granularity far coarser than a second, so two
// distinct binary contents written back-to-back could in principle share a
// (size, mtime) pair and be wrongly trusted as unchanged. That's the
// deliberate tradeoff here — it mirrors what `make` and most build caches
// already accept — and it's why `harness plugin probe` (which always
// re-probes and re-hashes) exists as the explicit, unambiguous refresh
// path after rebuilding a plugin binary.
func pluginBinaryIdentity(command []string, dir string) (path string, size int64, modTimeNS int64, err error) {
	path, err = plugin.ResolveExecutable(command, dir)
	if err != nil {
		return "", 0, 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, 0, err
	}
	return path, fi.Size(), fi.ModTime().UnixNano(), nil
}

// pluginScriptFiles resolves the command arguments AFTER command[0] that
// point at a real, regular file on disk — the case an interpreter-wrapped
// plugin hits, e.g. {"command":["bun","<repo>/.harness/plugins/guard.ts"]}:
// command[0] is a stable interpreter binary that pluginBinaryIdentity
// already tracks, but the script it runs is repo-owned and can change on
// every checkout, invisibly to that check alone.
//
// An argument that is not a file — a flag such as "--verbose", an option
// value, or a path that doesn't exist — is silently skipped rather than
// treated as an error or a missing file: only arguments that ARE files
// contribute to a plugin's identity, so a flag can never force a permanent
// cache miss (finding (c)). A plain, non-interpreter plugin (command ==
// [binary], no trailing arguments) resolves no script files at all, so its
// cache identity is exactly what pluginBinaryIdentity alone already gives
// it (finding (d)).
//
// Resolution mirrors ResolveExecutable's own rule for a path-like
// command[0]: an absolute argument is used as-is; anything else is joined
// against dir (falling back to the calling process's own working directory
// when dir is empty). A bare name is never looked up on PATH here — only
// command[0] is ever a PATH-resolved interpreter; a script argument is
// always a path relative to dir or the caller's cwd, exactly like a
// path-like command[0] would be.
func pluginScriptFiles(command []string, dir string) ([]pluginFileStat, error) {
	if len(command) < 2 {
		return nil, nil
	}
	var files []pluginFileStat
	for _, arg := range command[1:] {
		p := arg
		if !filepath.IsAbs(p) {
			base := dir
			if base == "" {
				wd, err := os.Getwd()
				if err != nil {
					return nil, err
				}
				base = wd
			} else {
				abs, err := filepath.Abs(base)
				if err != nil {
					return nil, err
				}
				base = abs
			}
			p = filepath.Join(base, p)
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		files = append(files, pluginFileStat{Path: p, Size: fi.Size(), ModTimeNS: fi.ModTime().UnixNano()})
	}
	return files, nil
}

// pluginFileStatsEqual reports whether two pluginScriptFiles results
// describe the same files at the same (size, mtime) in the same order.
// Order matters — command argument order is part of what actually
// executes — and a changed count (a script argument added or removed from
// config, or a file that started/stopped existing) is itself a mismatch.
func pluginFileStatsEqual(a, b []pluginFileStat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pluginScriptFilesHash hashes the content of every file pluginScriptFiles
// resolved, in order, the same way pluginBinaryHashAt hashes the
// executable: content is the ground truth for "did the script actually
// change," used only when the (size, mtime) fast path can't rule out a
// change (see buildPluginSpecs). Each file's path is folded in ahead of its
// content so two different paths that happen to hold identical bytes can
// never collide into the same combined hash. It reuses
// pluginBinaryHashCalls (rather than a second counter) since it is the same
// question tests care about: did this call actually read a file's content.
func pluginScriptFilesHash(files []pluginFileStat) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, f := range files {
		pluginBinaryHashCalls.Add(1)
		content, err := os.ReadFile(f.Path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d:%s\x00", len(f.Path), f.Path)
		h.Write(content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildPluginSpecs resolves a plugin.Spec (with its Manifest filled in) for
// every configured plugin, in config order (chain order is significant —
// see plugin/PROTOCOL.md). dirty reports whether the cache changed, so the
// caller knows to persist it.
//
// Cache lookup is two-level, in order of cost:
//  1. Key by name + a digest of Config/Env/Dir (pluginSpecDigest): any
//     change there is an automatic miss, since it can change what the live
//     plugin actually does regardless of its binary.
//  2. Within a key hit, stat (not hash) the resolved executable
//     (pluginBinaryIdentity) AND any script argument (pluginScriptFiles —
//     e.g. the ".ts" a fleet plugin like {"command":["bun","guard.ts"]}
//     runs) and compare (size, mtime) to the entry: a match on both trusts
//     the cached manifest without reading either file. A stat mismatch on
//     either side — or no entry — falls back to hashing that side's
//     content (pluginBinaryHashAt / pluginScriptFilesHash), and only a
//     hash mismatch actually re-probes; same content, just touched, just
//     refreshes the stat fields and keeps the cached manifest. This is the
//     fix for the defect where a repo-owned script's content could change
//     invisibly to cache validity: command[0] alone (a stable interpreter
//     binary) never reflects it, and Command was deliberately excluded
//     from pluginSpecDigest.
func buildPluginSpecs(ctx context.Context, plugins []config.PluginSpec, cache *pluginManifestCache) (specs []plugin.Spec, dirty bool, err error) {
	for _, p := range plugins {
		path, size, modTimeNS, ierr := pluginBinaryIdentity(p.Command, p.Dir)
		if ierr != nil {
			return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, ierr)
		}
		scriptFiles, serr := pluginScriptFiles(p.Command, p.Dir)
		if serr != nil {
			return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, serr)
		}
		specDigest, derr := pluginSpecDigest(p.Config, p.Env, p.Dir)
		if derr != nil {
			return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, derr)
		}
		key := pluginCacheKey(p.Name, specDigest)

		entry, ok := cache.Entries[key]
		needProbe := true
		var hash string
		var scriptHash string
		var hashedBinary, hashedScript bool
		if ok {
			binaryStatOK := entry.Size == size && entry.ModTimeNS == modTimeNS
			scriptStatOK := pluginFileStatsEqual(entry.ScriptFiles, scriptFiles)
			sameContent := true
			if !binaryStatOK {
				// Stat mismatch (touched or truly changed) — fall back to
				// content hash to tell those apart.
				var herr error
				hash, herr = pluginBinaryHashAt(path)
				if herr != nil {
					return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, herr)
				}
				hashedBinary = true
				if hash != entry.BinaryHash {
					sameContent = false
				}
			}
			if sameContent && !scriptStatOK {
				var herr error
				scriptHash, herr = pluginScriptFilesHash(scriptFiles)
				if herr != nil {
					return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, herr)
				}
				hashedScript = true
				if scriptHash != entry.ScriptHash {
					sameContent = false
				}
			}
			if binaryStatOK && scriptStatOK {
				// Both sides' stats match: trust the cached manifest
				// without reading either file.
				needProbe = false
			} else if sameContent {
				// Same content on any side that was hashed, just
				// touched: refresh the stat fields so the next startup
				// hits the no-hash fast path again, but keep the cached
				// manifest — no re-probe needed.
				if !hashedBinary {
					hash = entry.BinaryHash
				}
				if !hashedScript {
					scriptHash = entry.ScriptHash
				}
				entry.Size = size
				entry.ModTimeNS = modTimeNS
				entry.ScriptFiles = scriptFiles
				entry.ScriptHash = scriptHash
				cache.Entries[key] = entry
				dirty = true
				needProbe = false
			}
		}

		var manifest plugin.Manifest
		if !needProbe {
			manifest = entry.Manifest
		} else {
			if !hashedBinary {
				var herr error
				hash, herr = pluginBinaryHashAt(path)
				if herr != nil {
					return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, herr)
				}
			}
			if !hashedScript {
				var herr error
				scriptHash, herr = pluginScriptFilesHash(scriptFiles)
				if herr != nil {
					return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, herr)
				}
			}
			pctx, cancel := context.WithTimeout(ctx, pluginProbeTimeout)
			manifest, err = plugin.ProbeSpec(pctx, plugin.Spec{
				Command: p.Command,
				Env:     p.Env,
				Dir:     p.Dir,
				Config:  p.Config,
			})
			cancel()
			if err != nil {
				return nil, dirty, fmt.Errorf("plugin %s: probe: %w", p.Name, err)
			}
			if manifest.Name != p.Name {
				return nil, dirty, fmt.Errorf("plugin %s: manifest name %q does not match config", p.Name, manifest.Name)
			}
			cache.Entries[key] = pluginCacheEntry{
				BinaryHash:  hash,
				Size:        size,
				ModTimeNS:   modTimeNS,
				ScriptFiles: scriptFiles,
				ScriptHash:  scriptHash,
				Manifest:    manifest,
			}
			dirty = true
		}

		specs = append(specs, plugin.Spec{
			Command:  p.Command,
			Env:      p.Env,
			Dir:      p.Dir,
			Config:   p.Config,
			Manifest: manifest,
		})
	}
	return specs, dirty, nil
}

// buildPluginHost wires configured plugins into a *plugin.Host: it loads the
// on-disk manifest cache, probes any plugin missing from it (the only case
// where a plugin process is spawned before a session dispatches a hook to
// it), persists a refreshed cache, and constructs the Host. httpHeaders
// comes straight from config's plugin_http_headers and is passed through to
// plugin.Options.HTTPHeaders, which host.go already stamps into every
// plugin's InitializeParams.HTTPHeaders — no new stamping machinery here.
//
// It returns a nil Host (and nil error) when no plugins are configured.
// Callers must route the result through pluginHooks rather than assigning it
// to an engine.Hooks-typed field directly: a typed-nil *plugin.Host in an
// interface is not a nil interface.
func buildPluginHost(ctx context.Context, plugins []config.PluginSpec, harnessVersion, workDir string, httpHeaders map[string]string, client plugin.ClientAPI, serveURL, runToken string) (*plugin.Host, error) {
	if len(plugins) == 0 {
		return nil, nil
	}
	cachePath := pluginCachePath()
	cache, err := loadPluginManifestCache(cachePath)
	if err != nil {
		return nil, err
	}
	specs, dirty, err := buildPluginSpecs(ctx, plugins, cache)
	if err != nil {
		return nil, err
	}
	if dirty {
		if err := cache.save(cachePath); err != nil {
			return nil, err
		}
	}
	return plugin.NewHost(plugin.Options{
		Client:         client,
		ServeURL:       serveURL,
		RunToken:       runToken,
		HarnessVersion: harnessVersion,
		WorkspaceDir:   workDir,
		HTTPHeaders:    httpHeaders,
	}, specs...)
}

// pluginHooks adapts a possibly-nil *plugin.Host to engine.Hooks. Assigning a
// typed-nil *plugin.Host directly to an engine.Hooks-typed struct field
// produces a non-nil interface (the classic Go gotcha): every
// `cfg.Hooks != nil` check in the engine would then be true, and the first
// one to call a method on it would panic dereferencing a nil Host. Routing
// through this function keeps "no plugins configured" behaving exactly like
// today: a true nil interface, hooks disabled.
func pluginHooks(host *plugin.Host) engine.Hooks {
	if host == nil {
		return nil
	}
	return host
}

// pluginInfoFn adapts a possibly-nil *plugin.Host to
// server.Options.Plugins: the list of configured plugins a read of a
// NON-LIVE session reports (see that field's doc comment — such a read is
// answered from the session's metadata index, which has no Session to ask).
// A nil host reports no plugins, the same answer a session with no hooks
// gives through engine.Session.Plugins.
func pluginInfoFn(host *plugin.Host) func(string) []plugin.Info {
	if host == nil {
		return nil
	}
	// harness serve wires ONE host for every session it runs, so the id is
	// not consulted. The parameter exists for an embedder whose own
	// NewSession/LoadSession wrappers vary hooks per session.
	return func(string) []plugin.Info { return host.Plugins() }
}

// pluginCmd dispatches `harness plugin <subcommand>`.
func pluginCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: harness plugin probe")
	}
	switch args[0] {
	case "probe":
		return pluginProbeCmd(args[1:])
	default:
		return fmt.Errorf("unknown plugin subcommand %q (want: probe)", args[0])
	}
}

// pluginProbeCmd re-probes every configured plugin — always, unlike
// buildPluginHost's as-needed probing — and refreshes the on-disk manifest
// cache, printing each plugin's name and subscribed hooks. This is the
// explicit "refresh the cache" step: after rebuilding a plugin binary, or to
// confirm a newly-added plugin is wired correctly before it is ever used in
// a session.
func pluginProbeCmd(args []string) error {
	fs := flag.NewFlagSet("plugin probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Plugins) == 0 {
		fmt.Println("no plugins configured")
		return nil
	}
	cachePath := pluginCachePath()
	cache, err := loadPluginManifestCache(cachePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginProbeTimeout*time.Duration(len(cfg.Plugins)))
	defer cancel()
	for _, p := range cfg.Plugins {
		path, size, modTimeNS, err := pluginBinaryIdentity(p.Command, p.Dir)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		hash, err := pluginBinaryHashAt(path)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		scriptFiles, err := pluginScriptFiles(p.Command, p.Dir)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		scriptHash, err := pluginScriptFilesHash(scriptFiles)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		specDigest, err := pluginSpecDigest(p.Config, p.Env, p.Dir)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		pctx, pcancel := context.WithTimeout(ctx, pluginProbeTimeout)
		m, err := plugin.ProbeSpec(pctx, plugin.Spec{
			Command: p.Command,
			Env:     p.Env,
			Dir:     p.Dir,
			Config:  p.Config,
		})
		pcancel()
		if err != nil {
			return fmt.Errorf("plugin %s: probe: %w", p.Name, err)
		}
		if m.Name != p.Name {
			return fmt.Errorf("plugin %s: manifest name %q does not match config", p.Name, m.Name)
		}
		cache.Entries[pluginCacheKey(p.Name, specDigest)] = pluginCacheEntry{
			BinaryHash:  hash,
			Size:        size,
			ModTimeNS:   modTimeNS,
			ScriptFiles: scriptFiles,
			ScriptHash:  scriptHash,
			Manifest:    m,
		}
		hooks := make([]string, len(m.Hooks))
		for i, h := range m.Hooks {
			hooks[i] = string(h)
		}
		fmt.Printf("%s: %s\n", p.Name, strings.Join(hooks, ", "))
	}
	return cache.save(cachePath)
}
