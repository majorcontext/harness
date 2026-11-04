package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type packageExport struct{ ImportPath, Export string }

type generator struct {
	pkg   *types.Package
	names map[types.Type]string
	used  map[string]bool
	queue []types.Type
}

func main() {
	out := flag.String("out", "internal/eventlog", "output directory")
	check := flag.Bool("check", false, "check generated files")
	flag.Parse()
	if err := run(*out, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out string, check bool) error {
	pkg, err := load()
	if err != nil {
		return err
	}
	g := &generator{pkg: pkg, names: map[types.Type]string{}, used: map[string]bool{}}
	for _, n := range []string{"State", "LightState"} {
		obj := pkg.Scope().Lookup(n)
		if obj == nil {
			return fmt.Errorf("missing type %s", n)
		}
		g.add(obj.Type())
	}
	var files [][]byte
	for i := 0; i < len(g.queue); i++ {
		if i%20 == 0 {
			files = append(files, []byte(header))
		}
		text, err := g.emit(g.queue[i])
		if err != nil {
			return err
		}
		files[len(files)-1] = append(files[len(files)-1], text...)
	}
	if !check {
		if err := os.MkdirAll(out, 0755); err != nil {
			return err
		}
	}
	matches, err := filepath.Glob(filepath.Join(out, "light_codec_fields_*.go"))
	if err != nil {
		return err
	}
	if check && len(matches) != len(files) {
		return fmt.Errorf("checkpoint codec file count: got %d, want %d", len(matches), len(files))
	}
	for i, b := range files {
		b, err = format.Source(b)
		if err != nil {
			return fmt.Errorf("format part %d: %w", i, err)
		}
		name := filepath.Join(out, fmt.Sprintf("light_codec_fields_%02d.go", i))
		if check {
			old, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			if !bytes.Equal(b, old) {
				return fmt.Errorf("%s differs from generated checkpoint codec", name)
			}
		} else if err := os.WriteFile(name, b, 0644); err != nil {
			return err
		}
	}
	return nil
}

func load() (*types.Package, error) {
	cmd := exec.Command("go", "list", "-e", "-export", "-deps", "-json", "./internal/eventlog")
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	exports := map[string]string{}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	for {
		var p packageExport
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		exports[p.ImportPath] = p.Export
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "internal/eventlog", func(info os.FileInfo) bool {
		n := info.Name()
		return strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") && !strings.HasPrefix(n, "light_codec_fields_")
	}, 0)
	if err != nil {
		return nil, err
	}
	p := pkgs["eventlog"]
	if p == nil {
		return nil, fmt.Errorf("eventlog package not found")
	}
	names := make([]string, 0, len(p.Files))
	for n := range p.Files {
		names = append(names, n)
	}
	slicesSort(names)
	files := make([]*ast.File, 0, len(names))
	for _, n := range names {
		files = append(files, p.Files[n])
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) })
	cfg := types.Config{Importer: imp, Error: func(error) {}}
	pkg, _ := cfg.Check("github.com/majorcontext/harness/internal/eventlog", fset, files, nil)
	if pkg == nil {
		return nil, fmt.Errorf("cannot type check eventlog")
	}
	return pkg, nil
}

func slicesSort(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

const header = `package eventlog
import (
"encoding/json"
"github.com/majorcontext/harness/protocol"
"sort"
"time"
"unsafe"
)
var _ json.RawMessage
var _ time.Time
var _ = sort.Strings
var _ protocol.Settings
`
