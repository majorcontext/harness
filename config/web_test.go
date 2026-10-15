package config

import (
	"cmp"
	"strings"
	"testing"
)

func TestWebSpec(t *testing.T) {
	for _, tc := range []struct {
		name, user, project string
		wantFetch           bool
		wantErr             string
	}{
		{name: "absent is off"},
		{name: "user on", user: `{"web":{"fetch":true}}`, wantFetch: true},
		{name: "project false overrides user true", user: `{"web":{"fetch":true}}`, project: `{"web":{"fetch":false}}`},
		{name: "an unset project field inherits", user: `{"web":{"fetch":true}}`, project: `{"web":{}}`, wantFetch: true},
		{name: "an unknown key is rejected", user: `{"web":{"search":true}}`, wantErr: "search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, err := Parse("user.json", []byte(cmp.Or(tc.user, "{}")))
			if err == nil {
				var proj *Config
				if proj, err = Parse("project.json", []byte(cmp.Or(tc.project, "{}"))); err == nil {
					user = merge(user, proj)
				}
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want one with %q", err, tc.wantErr)
			}
			if tc.wantErr == "" && (err != nil || user.WebFetchEnabled() != tc.wantFetch) {
				t.Errorf("err = %v, WebFetchEnabled = %v, want %v", err, user.WebFetchEnabled(), tc.wantFetch)
			}
		})
	}
}
