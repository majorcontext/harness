package config

import "testing"

func TestCacheFinalMessageMergeAndClone(t *testing.T) {
	for _, value := range []bool{false, true} {
		for _, override := range []*bool{nil, new(false), new(true)} {
			user := &Config{Providers: map[string]Provider{"anthropic": {CacheTTL: "5m", CacheFinalMessage: &value}}}
			project := &Config{Providers: map[string]Provider{"anthropic": {CacheFinalMessage: override}}}
			got, err := mergeAndValidate(user, project)
			if err != nil {
				t.Fatal(err)
			}
			want := value
			if override != nil {
				want = *override
			}
			p := got.Providers["anthropic"]
			if *p.CacheFinalMessage != want || p.CacheTTL != "5m" {
				t.Fatalf("merged provider = %+v, want control = %v and 5m", p, want)
			}
			if p.CacheFinalMessage == user.Providers["anthropic"].CacheFinalMessage ||
				p.CacheFinalMessage == override || p.Clone().CacheFinalMessage == p.CacheFinalMessage {
				t.Fatal("merge or clone shares control")
			}
		}
	}
}
