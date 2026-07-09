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

// pluginManifestCache is the on-disk manifest cache named in AGENTS.md:
// "harness plugin install runs the binary once and caches its manifest ...
// keyed by binary hash". Entries are keyed by plugin name *and* a digest of
// the plugin's Config/Env/Dir (see pluginCacheKey/pluginSpecDigest) — not
// just the binary hash — so a renamed config entry, a plugin binary that
// changed since it was last probed, *or* a plugin whose config/env/dir
// changed without a binary rebuild, all re-probe rather than silently
// reusing a stale manifest.
type pluginManifestCache struct {
	Entries map[string]plugin.Manifest `json:"entries"`
}

// loadPluginManifestCache reads the on-disk manifest cache. A missing file
// is an empty cache (nothing has ever been probed). A present-but-corrupt
// file — most commonly a concurrent reader catching an in-progress write
// under the pre-atomic-rename save(), but any other corruption too — is
// treated the same as a cache miss: every plugin simply re-probes and the
// next save() overwrites the bad file. It is never a startup failure; only
// an I/O error opening the file (permissions, etc.) is.
func loadPluginManifestCache(path string) (*pluginManifestCache, error) {
	c := &pluginManifestCache{Entries: map[string]plugin.Manifest{}}
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
		return &pluginManifestCache{Entries: map[string]plugin.Manifest{}}, nil
	}
	if c.Entries == nil {
		c.Entries = map[string]plugin.Manifest{}
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
// order and identity are unambiguous) plus a digest folding together the
// binary hash and the plugin's Config/Env/Dir (see pluginCacheEntryDigest),
// so a rebuilt binary *or* a changed config/env/dir both invalidate the
// entry.
func pluginCacheKey(name, digest string) string { return name + "@" + digest }

// pluginCacheEntryDigest folds a plugin's binary hash together with a
// deterministic digest of its Config, Env, and Dir into the single value
// pluginCacheKey uses to identify a cache entry. Without Config/Env/Dir
// folded in here, a config.json edit that changes a plugin's config, env,
// or working directory — without touching its binary — would silently
// reuse the manifest probed under the old settings: the exact divergence
// between "what was probed" and "what actually runs" that the ProbeSpec fix
// (see buildPluginSpecs) closed for the probe call itself. This closes it
// for the cache key too.
//
// cfg is decoded into an untyped value before being folded into the
// digested struct so insignificant JSON differences in config.json (key
// order, whitespace) don't cause spurious cache misses: json.Marshal of a
// map[string]any sorts keys, so two byte-different-but-semantically-equal
// RawMessages produce the same digest.
func pluginCacheEntryDigest(hash string, cfg json.RawMessage, env []string, dir string) (string, error) {
	var cfgVal any
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &cfgVal); err != nil {
			return "", fmt.Errorf("plugin config: %w", err)
		}
	}
	b, err := json.Marshal(struct {
		BinaryHash string   `json:"binary_hash"`
		Config     any      `json:"config"`
		Env        []string `json:"env"`
		Dir        string   `json:"dir"`
	}{BinaryHash: hash, Config: cfgVal, Env: env, Dir: dir})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// pluginBinaryHash hashes the plugin's executable so a changed binary
// invalidates its cache entry, without spawning it. command[0] is resolved
// via plugin.ResolveExecutable — the exact same rules (and the exact same
// function) the real spawn path (plugin.Host's instance.dial) uses, keyed
// off the same dir — so the hash can never track a different file than the
// one that executes.
func pluginBinaryHash(command []string, dir string) (string, error) {
	path, err := plugin.ResolveExecutable(command, dir)
	if err != nil {
		return "", err
	}
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

// buildPluginSpecs resolves a plugin.Spec (with its Manifest filled in) for
// every configured plugin, in config order (chain order is significant —
// see plugin/PROTOCOL.md). A plugin whose binary hash is not yet in cache is
// probed (plugin.Probe: spawn, initialize, shutdown) to populate it; dirty
// reports whether the cache changed, so the caller knows to persist it.
func buildPluginSpecs(ctx context.Context, plugins []config.PluginSpec, cache *pluginManifestCache) (specs []plugin.Spec, dirty bool, err error) {
	for _, p := range plugins {
		hash, herr := pluginBinaryHash(p.Command, p.Dir)
		if herr != nil {
			return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, herr)
		}
		digest, derr := pluginCacheEntryDigest(hash, p.Config, p.Env, p.Dir)
		if derr != nil {
			return nil, dirty, fmt.Errorf("plugin %s: %w", p.Name, derr)
		}
		key := pluginCacheKey(p.Name, digest)
		manifest, ok := cache.Entries[key]
		if !ok {
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
			cache.Entries[key] = manifest
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
func buildPluginHost(ctx context.Context, plugins []config.PluginSpec, harnessVersion, workDir string, httpHeaders map[string]string) (*plugin.Host, error) {
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
		hash, err := pluginBinaryHash(p.Command, p.Dir)
		if err != nil {
			return fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		digest, err := pluginCacheEntryDigest(hash, p.Config, p.Env, p.Dir)
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
		cache.Entries[pluginCacheKey(p.Name, digest)] = m
		hooks := make([]string, len(m.Hooks))
		for i, h := range m.Hooks {
			hooks[i] = string(h)
		}
		fmt.Printf("%s: %s\n", p.Name, strings.Join(hooks, ", "))
	}
	return cache.save(cachePath)
}
