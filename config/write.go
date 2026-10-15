package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SetModel writes model into the config file at path when the file sets none, and reports whether it wrote.
// It creates a missing file, inserts the key into the text of an existing file and so keeps every other byte
// and the mode, and replaces the file in one rename. A file that holds an empty "model" key, of any letter case, is the one case
// that is written again with sorted keys. A file that does not parse, holds no object, or would not load back with the model is left as it is and is an error. A
// symlink is written through, and a symlink to a missing file is an error.
func SetModel(path, model string) (bool, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return false, fmt.Errorf("config: %s is a symlink to a file that does not exist", path)
		}
		path = real
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	mode := os.FileMode(0o600)
	var out string
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return false, err
		}
		out = newDocument(raw)
	case err != nil:
		return false, err
	default:
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if out, err = withModel(path, data, raw); err != nil || out == "" {
			return false, err
		}
	}
	return true, replaceFile(path, []byte(out), mode)
}

// withModel returns the new text of the file, or "" when the file already sets a model.
func withModel(path string, data, raw []byte) (string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return newDocument(raw), nil
	}
	cfg, err := Parse(path, data)
	if err != nil || cfg.Model != "" {
		return "", err
	}
	if bytes.TrimSpace(data)[0] != '{' {
		return "", fmt.Errorf("config: %s holds no JSON object", path)
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("config: parsing %s: %w", path, err)
	}
	var want string
	_ = json.Unmarshal(raw, &want)
	has := false
	for k := range doc {
		if strings.EqualFold(k, "model") {
			delete(doc, k)
			has = true
		}
	}
	var out string
	if !has {
		out = insertModel(string(data), raw)
	} else {
		doc["model"] = raw
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return "", err
		}
		out = string(b) + "\n"
	}
	if back, err := Parse(path, []byte(out)); err != nil || back.Model != want {
		return "", fmt.Errorf("config: cannot set model in %s without changing its other keys", path)
	}
	return out, nil
}

func newDocument(raw []byte) string { return "{\n  \"model\": " + string(raw) + "\n}\n" }

// insertModel puts the key "model" first in the top-level object of text, with the indent of the next key.
func insertModel(text string, raw []byte) string {
	open := strings.IndexByte(text, '{') + 1
	rest := text[open:]
	ws := rest[:len(rest)-len(strings.TrimLeft(rest, " \t\r\n"))]
	entry := `"model": ` + string(raw)
	switch {
	case strings.HasPrefix(rest[len(ws):], "}"):
		return text[:open] + entry + text[open:]
	case strings.Contains(ws, "\n"):
		return text[:open] + ws + entry + "," + text[open:]
	}
	return text[:open] + entry + "," + text[open:]
}

// replaceFile writes data to a temporary file beside path, syncs it, and renames it over path.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
