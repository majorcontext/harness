package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// runAuth answers `claude auth login|logout|status`. FAKE_CLAUDE_AUTH is a file that holds the login state
// ("in" or "out"; absent means logged in), and FAKE_CLAUDE_AUTH_LOG receives the arguments of each call.
// Neither holds a token.
func runAuth(args []string) int {
	if path := os.Getenv("FAKE_CLAUDE_AUTH_LOG"); path != "" {
		appendFile(path, strings.Join(args, " ")+"\n")
	}
	set := func(state string) {
		if path := os.Getenv("FAKE_CLAUDE_AUTH"); path != "" {
			_ = os.WriteFile(path, []byte(state), 0o644)
		}
	}
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "login":
		set("in")
	case "logout":
		set("out")
	case "status":
		in := true
		if b, err := os.ReadFile(os.Getenv("FAKE_CLAUDE_AUTH")); err == nil {
			in = strings.TrimSpace(string(b)) != "out"
		}
		if len(args) > 1 && args[1] == "--json" {
			b, _ := json.Marshal(obj{"loggedIn": in, "authMethod": "claude.ai"})
			fmt.Println(string(b))
		} else if in {
			fmt.Println("Logged in")
		} else {
			fmt.Println("Not logged in")
		}
		if !in {
			return 1
		}
	default:
		return 2
	}
	return 0
}
