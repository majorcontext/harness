package command

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrNotCommand reports that a line is ordinary text. Resolution.Text
// holds what the caller must send.
var ErrNotCommand = errors.New("command: not a command")

// UnknownCommandError reports a /name with no entry. A caller must NOT
// send the line to the model as text.
type UnknownCommandError struct{ Name string }

func (e *UnknownCommandError) Error() string {
	return fmt.Sprintf("command: unknown command %q", e.Name)
}

// Resolution is what one input line resolves to.
type Resolution struct {
	Kind Kind
	Spec *Spec
	Op   Op
	Args map[string]any
	Text string
}

// Resolve parses one input line. It performs no I/O and calls no route.
//
// Four rules protect a destructive command from a bad parse:
//  1. surplus input is an error; Resolve never drops part of a line
//  2. a command is the whole input: one line, nothing after it
//  3. //name is the literal text /name
//  4. a line that parses only after dropping text is not a command
func (r *Registry) Resolve(line string) (Resolution, error) {
	// Rule 2: a multi-line message is text.
	if strings.ContainsAny(line, "\n\r") {
		return Resolution{Text: line}, ErrNotCommand
	}
	// Rule 3: // escapes to a literal leading slash.
	if strings.HasPrefix(line, "//") {
		return Resolution{Text: line[1:]}, ErrNotCommand
	}
	if !strings.HasPrefix(line, "/") {
		return Resolution{Text: line}, ErrNotCommand
	}
	body := line[1:]
	if lead, _ := utf8.DecodeRuneInString(body); body != "" && unicode.IsSpace(lead) {
		return Resolution{Text: line}, ErrNotCommand
	}
	name, rest := body, ""
	if i := strings.IndexFunc(body, unicode.IsSpace); i >= 0 {
		_, size := utf8.DecodeRuneInString(body[i:])
		name, rest = body[:i], body[i+size:]
	}
	if name == "" {
		return Resolution{Text: line}, ErrNotCommand
	}
	spec, ok := r.Lookup(name)
	if !ok {
		return Resolution{}, &UnknownCommandError{Name: name}
	}
	args, err := bindArgs(spec, name, strings.TrimSpace(rest))
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Kind: spec.Kind, Spec: spec, Op: spec.Op, Args: args}, nil
}

// cutSpace splits s at the first Unicode whitespace rune, mirroring Resolve's
// own name/rest split (the IndexFunc/DecodeRuneInString pair above) instead
// of strings.Cut's ASCII-only " ". Without this, a folded non-ASCII space
// (NBSP, an ideographic space) survives into a positional field bindArgs
// binds verbatim, splitting on the wrong boundary or not at all.
func cutSpace(s string) (field, rest string) {
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	return s[:i], s[i+size:]
}

// bindArgs binds the remainder of a line to a Spec's positional
// arguments. Rule 1 lives here: leftover input is an error. typedName
// is the name (or alias) the user actually wrote, used in error text
// instead of spec.Name so a message never names a command the user
// did not type.
func bindArgs(spec *Spec, typedName, rest string) (map[string]any, error) {
	args := map[string]any{}
	if len(spec.Args) == 0 {
		if rest != "" {
			return nil, fmt.Errorf("command: /%s takes no arguments, got %q", typedName, rest)
		}
		return args, nil
	}
	for i, a := range spec.Args {
		if a.Type == ArgRest {
			if rest == "" {
				if a.Optional {
					return args, nil
				}
				return nil, fmt.Errorf("command: /%s needs %s", typedName, a.Name)
			}
			args[a.Name] = rest
			return args, nil
		}
		var field string
		field, rest = cutSpace(rest)
		rest = strings.TrimSpace(rest)
		if field == "" {
			if a.Optional {
				continue
			}
			return nil, fmt.Errorf("command: /%s needs %s", typedName, a.Name)
		}
		switch a.Type {
		case ArgInt:
			n, err := strconv.Atoi(field)
			if err != nil {
				return nil, fmt.Errorf("command: /%s %s must be a number, got %q", typedName, a.Name, field)
			}
			args[a.Name] = n
		default:
			args[a.Name] = field
		}
		if i == len(spec.Args)-1 && rest != "" {
			return nil, fmt.Errorf("command: /%s takes %d argument(s), got extra %q", typedName, len(spec.Args), rest)
		}
	}
	return args, nil
}
