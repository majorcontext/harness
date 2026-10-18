package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// RevKind names the kind of a revision. Only a VCS engine gives an ID meaning.
type RevKind string

// Revision kinds. RevBranch is a git branch or a jj bookmark, and RevChange is
// a jj change.
const (
	RevBranch RevKind = "branch"
	RevTag    RevKind = "tag"
	RevCommit RevKind = "commit"
	RevChange RevKind = "change"
)

// Rev names a revision or a change. Its text form, and its JSON form, is
// "<kind>:<id>". The ID may contain ':' because the text splits at the first
// one. The zero Rev has the empty text and, in a field tagged omitzero, no
// JSON member.
type Rev struct {
	Kind RevKind
	ID   string
}

// ErrInvalidRev is the error of a revision text that ParseRev refuses.
var ErrInvalidRev = errors.New("invalid revision")

// ParseRev parses "<kind>:<id>". The kind must be one of the RevKind
// constants and the ID must not be empty.
func ParseRev(s string) (Rev, error) {
	kind, id, ok := strings.Cut(s, ":")
	if !ok {
		return Rev{}, fmt.Errorf("%w: %q has no ':'", ErrInvalidRev, s)
	}
	switch k := RevKind(kind); k {
	case RevBranch, RevTag, RevCommit, RevChange:
		if id == "" {
			return Rev{}, fmt.Errorf("%w: %q has an empty ID", ErrInvalidRev, s)
		}
		return Rev{Kind: k, ID: id}, nil
	}
	return Rev{}, fmt.Errorf("%w: %q has the unknown kind %q", ErrInvalidRev, s, kind)
}

// String returns the text form, or "" for the zero Rev.
func (r Rev) String() string {
	if r == (Rev{}) {
		return ""
	}
	return string(r.Kind) + ":" + r.ID
}

// MarshalText returns the text form. It fails for a Rev that ParseRev would
// refuse, except the zero Rev.
func (r Rev) MarshalText() ([]byte, error) {
	if r == (Rev{}) {
		return []byte{}, nil
	}
	if _, err := ParseRev(r.String()); err != nil {
		return nil, err
	}
	return []byte(r.String()), nil
}

// UnmarshalText parses the text form. The empty text is the zero Rev.
func (r *Rev) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*r = Rev{}
		return nil
	}
	p, err := ParseRev(string(text))
	if err != nil {
		return err
	}
	*r = p
	return nil
}
