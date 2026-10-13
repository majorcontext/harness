// Command wasmsize builds cmd/harness-wasm for GOOS=js GOARCH=wasm and fails
// when the module is larger than the budget of testdata/wasm-size-budget.txt.
// Run it from the repository root.
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type sizes struct {
	Raw  int64
	Gzip int64
}

func main() {
	budgetFile := flag.String("budget", "testdata/wasm-size-budget.txt", "budget file")
	flag.Parse()
	if err := run(*budgetFile); err != nil {
		fmt.Fprintln(os.Stderr, "wasmsize:", err)
		os.Exit(1)
	}
}

func run(budgetFile string) error {
	budget, err := readBudget(budgetFile)
	if err != nil {
		return err
	}
	got, err := measure()
	if err != nil {
		return err
	}
	fmt.Printf("raw  %d bytes (budget %d)\ngzip %d bytes (budget %d)\n", got.Raw, budget.Raw, got.Gzip, budget.Gzip)
	var over []string
	if got.Raw > budget.Raw {
		over = append(over, fmt.Sprintf("raw is %d bytes over", got.Raw-budget.Raw))
	}
	if got.Gzip > budget.Gzip {
		over = append(over, fmt.Sprintf("gzip is %d bytes over", got.Gzip-budget.Gzip))
	}
	if len(over) > 0 {
		return fmt.Errorf("module exceeds the budget in %s: %s", budgetFile, strings.Join(over, ", "))
	}
	return nil
}

func readBudget(path string) (sizes, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sizes{}, err
	}
	var b sizes
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		n, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if !ok || perr != nil {
			return sizes{}, fmt.Errorf("%s: bad line %q", path, line)
		}
		switch key {
		case "raw":
			b.Raw = n
		case "gzip":
			b.Gzip = n
		default:
			return sizes{}, fmt.Errorf("%s: unknown key %q", path, key)
		}
	}
	if b.Raw <= 0 || b.Gzip <= 0 {
		return sizes{}, fmt.Errorf("%s: raw and gzip budgets are required", path)
	}
	return b, nil
}

func measure() (sizes, error) {
	dir, err := os.MkdirTemp("", "wasmsize")
	if err != nil {
		return sizes{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, "harness.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/harness-wasm")
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return sizes{}, fmt.Errorf("build: %w", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return sizes{}, err
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return sizes{}, err
	}
	if _, err := zw.Write(data); err != nil {
		return sizes{}, err
	}
	if err := zw.Close(); err != nil {
		return sizes{}, err
	}
	return sizes{Raw: int64(len(data)), Gzip: int64(buf.Len())}, nil
}
