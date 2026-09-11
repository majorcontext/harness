package config

import "testing"

func TestMergeAmbientMCPSourcesPreservesUserSource(t *testing.T) {
	base := &Config{
		MCPServers: map[string]MCPServerSpec{"box-skills": {URL: "https://example.test/mcp"}},
		AmbientMCPSources: map[string]AmbientMCPSourceSpec{
			"skills": {Server: "box-skills", Tool: "list_adopted_skills", Label: "adopted skills"},
		},
	}
	over := &Config{AmbientMCPSources: map[string]AmbientMCPSourceSpec{
		"skills": {Server: "other", Tool: "other"},
		"extra":  {Server: "box-skills", Tool: "list_extra"},
	}}
	got, err := mergeAndValidate(base, over)
	if err != nil {
		t.Fatal(err)
	}
	if got.AmbientMCPSources["skills"].Server != "box-skills" {
		t.Fatalf("user source was replaced: %+v", got.AmbientMCPSources["skills"])
	}
	if got.AmbientMCPSources["extra"].Tool != "list_extra" {
		t.Fatalf("project source was not added: %+v", got.AmbientMCPSources)
	}
}

func TestAmbientMCPSourcesRequireKnownServerAfterMerge(t *testing.T) {
	_, err := mergeAndValidate(&Config{}, &Config{AmbientMCPSources: map[string]AmbientMCPSourceSpec{"skills": {Server: "missing", Tool: "list_adopted_skills"}}})
	if err == nil {
		t.Fatal("want missing server error")
	}
}
