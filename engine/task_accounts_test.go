package engine

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestTaskAccountSelectionAllocatesFirstExplicitSelection(t *testing.T) {
	routes := map[string]AccountRoutingConfig{"codex": {Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want map[string]string
	}{
		{name: "string", raw: json.RawMessage(`"acct_first"`), want: map[string]string{"codex": "acct_first"}},
		{name: "map", raw: json.RawMessage(`{"codex":"acct_first"}`), want: map[string]string{"codex": "acct_first"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTaskAccountSelection(tc.raw, "codex", nil, routes)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selection = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestTaskAccountSelectionRejectsInvalidShapesAndIDs(t *testing.T) {
	routes := map[string]AccountRoutingConfig{"codex": {Vendor: "codex"}}
	for _, raw := range []string{`null`, `[]`, `1`, `{"codex":null}`, `{"other":"acct_x"}`, `{"codex":"acct_bad-id"}`, `"acct_bad-id"`} {
		if _, err := resolveTaskAccountSelection(json.RawMessage(raw), "codex", nil, routes); err == nil {
			t.Errorf("resolveTaskAccountSelection(%s) succeeded", raw)
		}
	}
	if _, err := resolveTaskAccountSelection(json.RawMessage(`"acct_x"`), "other", nil, routes); err == nil {
		t.Fatal("string account on unsupported provider succeeded")
	}
}

func TestTaskAccountSelectionResolvesStringVendorAndInheritsOmittedMap(t *testing.T) {
	parent := map[string]string{"claude": "acct_claude", "codex": "acct_old"}
	routes := map[string]AccountRoutingConfig{"codex": {Vendor: "codex", ProxyURLEnv: "HTTPS_PROXY", Protocol: "boxes-v1"}}

	inherited, err := resolveTaskAccountSelection(nil, "codex", parent, routes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inherited, parent) {
		t.Fatalf("omitted account selection = %#v, want inherited %#v", inherited, parent)
	}

	selected, err := resolveTaskAccountSelection(json.RawMessage(`""`), "codex", parent, routes)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"claude": "acct_claude", "codex": ""}
	if !reflect.DeepEqual(selected, want) {
		t.Fatalf("explicit empty account selection = %#v, want %#v", selected, want)
	}
}
