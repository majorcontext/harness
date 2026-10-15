package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SetModel writes model into the config file at path when the file sets none, and reports whether it wrote.
// It creates a missing file, keeps every other key and the mode of an existing file, and replaces the file
// in one rename. A file that does not parse is left as it is and is an error.
func SetModel(path, model string) (bool, error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	data, err := os.ReadFile(path)
	mode := os.FileMode(0o600)
	doc := map[string]json.RawMessage{}
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if len(bytes.TrimSpace(data)) > 0 {
			cfg, err := Parse(path, data)
			if err != nil {
				return false, err
			}
			if cfg.Model != "" {
				return false, nil
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				return false, fmt.Errorf("config: parsing %s: %w", path, err)
			}
		}
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return false, err
	}
	doc["model"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	return true, replaceFile(path, append(out, '\n'), mode)
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
