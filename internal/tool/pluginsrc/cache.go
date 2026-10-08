package pluginsrc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/plugin"
)

// cache holds the probed manifest of each plugin, in the file format that the
// engine wrote, so a file written by either one serves both.
type cache struct {
	Entries map[string]cacheEntry `json:"entries"`
}

// cacheEntry is one manifest and the identity of the files that produced it:
// the executable, and each script argument that is a regular file. The size
// and the mtime decide a hit with no read; a change of either falls back to the
// content hash, and only a change of content probes again.
type cacheEntry struct {
	BinaryHash  string          `json:"binary_hash"`
	Size        int64           `json:"size"`
	ModTimeNS   int64           `json:"mtime_unix_nano"`
	ScriptFiles []fileStat      `json:"script_files,omitempty"`
	ScriptHash  string          `json:"script_hash,omitempty"`
	Manifest    plugin.Manifest `json:"manifest"`
}

type fileStat struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	ModTimeNS int64  `json:"mtime_unix_nano"`
}

// loadCache reads path. A missing file is an empty cache, and so is a file
// that does not decode: every plugin probes again and the next save replaces it.
func loadCache(path string) (*cache, error) {
	c := &cache{Entries: map[string]cacheEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		slog.Warn("pluginsrc: plugin cache ignored, probing again", "path", path, "err", err)
		return &cache{Entries: map[string]cacheEntry{}}, nil
	}
	if c.Entries == nil {
		c.Entries = map[string]cacheEntry{}
	}
	return c, nil
}

// save replaces path atomically, so a reader sees the old file or the new one.
func (c *cache) save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".plugin_cache-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Sync(), tmp.Close(), os.Chmod(tmp.Name(), 0o644)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// digest identifies what a binary hash does not cover: the config, the
// environment, and the working directory of a plugin. The config decodes
// first, so key order and spacing never cause a miss.
func digest(ps config.PluginSpec) (string, error) {
	var cfg any
	if len(ps.Config) > 0 {
		if err := json.Unmarshal(ps.Config, &cfg); err != nil {
			return "", fmt.Errorf("plugin config: %w", err)
		}
	}
	b, err := json.Marshal(struct {
		Config any      `json:"config"`
		Env    []string `json:"env"`
		Dir    string   `json:"dir"`
	}{cfg, ps.Env, ps.Dir})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// scriptFiles returns the arguments after command[0] that are regular files,
// so an interpreter-wrapped plugin is keyed by its script too. A relative
// argument joins dir, or the working directory of the process when dir is empty.
func scriptFiles(command []string, dir string) ([]fileStat, error) {
	if len(command) < 2 {
		return nil, nil
	}
	base := dir
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		base = wd
	} else if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	var files []fileStat
	for _, arg := range command[1:] {
		p := arg
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			files = append(files, fileStat{p, fi.Size(), fi.ModTime().UnixNano()})
		}
	}
	return files, nil
}

// scriptsHash hashes the path and the content of each file, in order.
func scriptsHash(files []fileStat) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, f := range files {
		content, err := os.ReadFile(f.Path)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%d:%s\x00", len(f.Path), f.Path)
		_, _ = h.Write(content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// manifest returns the manifest of ps from the cache when the plugin has not
// changed, else from a new probe, which it stores in c. changed reports a write
// to c. With refresh, it always probes.
func (c *cache) manifest(probe probeFunc, ps config.PluginSpec, refresh bool) (m plugin.Manifest, changed bool, err error) {
	path, err := plugin.ResolveExecutable(ps.Command, ps.Dir)
	if err != nil {
		return m, false, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return m, false, err
	}
	files, err := scriptFiles(ps.Command, ps.Dir)
	if err != nil {
		return m, false, err
	}
	sum, err := digest(ps)
	if err != nil {
		return m, false, err
	}
	key := ps.Name + "@" + sum
	e, ok := c.Entries[key]
	if ok && !refresh {
		binaryStat := e.Size == fi.Size() && e.ModTimeNS == fi.ModTime().UnixNano()
		scriptStat := slices.Equal(e.ScriptFiles, files)
		same := true
		if !binaryStat {
			h, err := hashFile(path)
			if err != nil {
				return m, false, err
			}
			same = h == e.BinaryHash
		}
		if same && !scriptStat {
			h, err := scriptsHash(files)
			if err != nil {
				return m, false, err
			}
			same = h == e.ScriptHash
		}
		switch {
		case same && binaryStat && scriptStat:
			return e.Manifest, false, nil
		case same:
			e.Size, e.ModTimeNS, e.ScriptFiles = fi.Size(), fi.ModTime().UnixNano(), files
			c.Entries[key] = e
			return e.Manifest, true, nil
		}
	}
	bh, err := hashFile(path)
	if err != nil {
		return m, false, err
	}
	sh, err := scriptsHash(files)
	if err != nil {
		return m, false, err
	}
	m, err = probe(plugin.Spec{Command: ps.Command, Env: ps.Env, Dir: ps.Dir, Config: ps.Config})
	if err != nil {
		return m, false, err
	}
	if m.Name != ps.Name {
		return m, false, fmt.Errorf("manifest name %q does not match the config", m.Name)
	}
	c.Entries[key] = cacheEntry{BinaryHash: bh, Size: fi.Size(), ModTimeNS: fi.ModTime().UnixNano(), ScriptFiles: files, ScriptHash: sh, Manifest: m}
	return m, true, nil
}

// probeFunc reads the manifest of a plugin with one bounded probe.
type probeFunc func(plugin.Spec) (plugin.Manifest, error)
