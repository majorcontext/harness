package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergePlugins(t *testing.T) {
	specs := func(names string) (out []PluginSpec) {
		for _, n := range strings.Fields(names) {
			out = append(out, PluginSpec{Name: n, Command: []string{n}})
		}
		return out
	}
	for _, tc := range []struct{ name, user, proj, want string }{
		{"two each keep their own order", "u1 u2", "p1 p2", "p1 p2 u1 u2"},
		{"empty project keeps user", "u", "", "u"},
		{"neither gives none", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, p := range merge(&Config{Plugins: specs(tc.user)}, &Config{Plugins: specs(tc.proj)}).Plugins {
				got = append(got, p.Name)
			}
			if g := strings.Join(got, " "); g != tc.want {
				t.Errorf("merged Plugins = %q, want %q", g, tc.want)
			}
		})
	}
}

func TestLoadProjectRejectsPluginInBothFiles(t *testing.T) {
	dir := t.TempDir()
	userPath, projPath := filepath.Join(dir, "config.json"), filepath.Join(dir, ".harness.json")
	writeFile(t, userPath, `{"plugins": [{"name": "x", "command": ["a"]}]}`)
	writeFile(t, projPath, `{"plugins": [{"name": "x", "command": ["b"]}]}`)
	_, err := LoadProject(userPath, dir)
	if want := fmt.Sprintf("plugins: plugin %q is in both %s and %s", "x", userPath, projPath); err == nil || err.Error() != want {
		t.Errorf("LoadProject error = %v, want %q", err, want)
	}
}
