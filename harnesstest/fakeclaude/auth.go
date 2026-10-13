package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// authOut says that FAKE_CLAUDE_AUTH holds the state "out".
func authOut() bool {
	b, err := os.ReadFile(os.Getenv("FAKE_CLAUDE_AUTH"))
	return err == nil && strings.TrimSpace(string(b)) == "out"
}

// holdLogin writes its pid and the pid of a child to the file FAKE_CLAUDE_AUTH_HOLD, then waits for ever, as a login
// that waits for a browser does. The child makes the process tree that a cancel must end.
func holdLogin(path string) {
	child := exec.Command(os.Args[0], "auth", "hold")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)), 0o644)
	sleepForever()
}

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
	case "hold":
		sleepForever()
	case "login":
		if path := os.Getenv("FAKE_CLAUDE_AUTH_HOLD"); path != "" {
			holdLogin(path)
		}
		set("in")
	case "logout":
		set("out")
	case "status":
		in := !authOut()
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

// sleepForever blocks in a way the runtime does not report as a deadlock.
func sleepForever() {
	for {
		time.Sleep(time.Hour)
	}
}
