// Command harness-migrate converts the session journals of the engine to
// event logs, once, at the cutover. It changes no old file.
//
//	harness-migrate -from <engine session dir> -to <store root>
//	harness-migrate -archive <sessions.tar.zst> -out <sessions.tar.zst> [-dir sessions] [-store sessions]
//
// It prints one line for each session and exits 1 when a session fails.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/migrate"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelWarn)
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "harness-migrate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("harness-migrate", flag.ContinueOnError)
	from := fs.String("from", "", "engine session directory")
	to := fs.String("to", "", "DiskStore root of the converted logs")
	archive := fs.String("archive", "", "sessions.tar.zst export to read")
	dst := fs.String("out", "", "sessions.tar.zst to write")
	dir := fs.String("dir", "sessions", "engine session directory inside the archive")
	store := fs.String("store", "sessions", "DiskStore root inside the archive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var results []migrate.Result
	var err error
	switch {
	case *from != "" && *to != "" && *archive == "":
		results, err = migrate.Dir(ctx, *from, harness.NewDiskStore(*to))
	case *archive != "" && *dst != "" && *from == "":
		results, err = convertArchive(ctx, *archive, *dst, *dir, *store)
	default:
		return errors.New("give -from and -to, or -archive and -out")
	}
	if err != nil {
		return err
	}
	for _, r := range results {
		switch {
		case r.Err != nil:
			fmt.Fprintf(out, "FAILED %s: %v\n", r.Session, r.Err)
		case r.Skipped:
			fmt.Fprintf(out, "skipped %s: the store holds it\n", r.Session)
		default:
			fmt.Fprintf(out, "converted %s: %d messages\n", r.Session, r.Messages)
		}
	}
	if n := len(migrate.Failed(results)); n > 0 {
		return fmt.Errorf("%d of %d sessions failed", n, len(results))
	}
	return nil
}

// convertArchive writes the converted archive next to dst, then renames it,
// so dst never holds a partial archive.
func convertArchive(ctx context.Context, src, dst, dir, store string) ([]migrate.Result, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".harness-migrate-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	results, err := migrate.Archive(ctx, in, tmp, dir, store)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return results, os.Rename(tmp.Name(), dst)
}
