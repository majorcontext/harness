package protocol_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func TestRevTextRoundTripsAndRefusesBadText(t *testing.T) {
	for _, s := range []string{"branch:main", "tag:v1", "commit:abc123", "change:kkmpptxz", "op:9f8e", "branch:feat/a:b"} {
		r, err := protocol.ParseRev(s)
		if err != nil || r.String() != s {
			t.Errorf("ParseRev(%q) = %v, %v; String %q", s, r, err, r.String())
		}
	}
	if r, _ := protocol.ParseRev("branch:a:b"); r.ID != "a:b" {
		t.Errorf("ID = %q, want the text after the first colon", r.ID)
	}
	for _, s := range []string{"", "main", "commit:", ":abc", "Commit:abc", "ref:abc"} {
		if _, err := protocol.ParseRev(s); !errors.Is(err, protocol.ErrInvalidRev) {
			t.Errorf("ParseRev(%q) error = %v, want ErrInvalidRev", s, err)
		}
	}
}

func TestRevJSONIsItsTextAndTheZeroRevIsOmitted(t *testing.T) {
	type holder struct {
		Rev protocol.Rev `json:"rev,omitzero"`
	}
	b, err := json.Marshal(holder{Rev: protocol.Rev{Kind: protocol.RevCommit, ID: "abc"}})
	if err != nil || string(b) != `{"rev":"commit:abc"}` {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
	var h holder
	if err := json.Unmarshal(b, &h); err != nil || h.Rev != (protocol.Rev{Kind: protocol.RevCommit, ID: "abc"}) {
		t.Fatalf("Unmarshal = %v, %v", h, err)
	}
	if b, _ := json.Marshal(holder{}); string(b) != `{}` {
		t.Errorf("zero Rev marshals as %s, want {}", b)
	}
	if err := json.Unmarshal([]byte(`{"rev":"bogus:x"}`), &h); err == nil {
		t.Error("Unmarshal accepted an unknown kind")
	}
	if _, err := json.Marshal(protocol.Rev{Kind: "bogus", ID: "x"}); err == nil {
		t.Error("Marshal accepted an unknown kind")
	}
}
