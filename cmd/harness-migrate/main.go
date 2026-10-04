// Command harness-migrate converts the session journals of the engine to
// event logs, once, at the cutover. It changes no old file.
//
//	harness-migrate -from <engine session dir> -to <store root> [-model provider/model]
//	harness-migrate -archive <sessions.tar.zst> -out <sessions.tar.zst> [-dir sessions] [-store sessions] [-model provider/model]
//
// It prints one line for each session and exits 1 when a session fails.
// -model is the model of a journal that names none, as the engine gives it
// the model of its server. For an archive, it also prints the size and the
// base64 MD5 of the new archive, which the manifest.json of the archive
// object must then name.
package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/message"
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
	fallback := fs.String("model", "", "provider/model of a journal that names no model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var model message.ModelRef
	if *fallback != "" {
		m, err := message.ParseModelRef(*fallback)
		if err != nil {
			return err
		}
		model = m
	}
	var results []migrate.Result
	var err error
	switch {
	case *from != "" && *to != "" && *archive == "":
		if _, err := os.Stat(*from); err != nil {
			return err
		}
		results, err = migrate.Dir(ctx, *from, harness.NewDiskStore(*to), model)
	case *archive != "" && *dst != "" && *from == "":
		results, err = convertArchive(ctx, out, *archive, *dst, *dir, *store, model)
	default:
		return errors.New("give -from and -to, or -archive and -out")
	}
	if err != nil {
		return err
	}
	for _, r := range results {
		var err error
		switch {
		case r.Err != nil:
			_, err = fmt.Fprintf(out, "FAILED %s: %v\n", r.Session, r.Err)
		case r.Skipped:
			_, err = fmt.Fprintf(out, "skipped %s: the store holds it\n", r.Session)
		default:
			_, err = fmt.Fprintf(out, "converted %s: %d messages\n", r.Session, r.Messages)
		}
		if err != nil {
			return err
		}
	}
	if n := len(migrate.Failed(results)); n > 0 {
		return fmt.Errorf("%d of %d sessions failed", n, len(results))
	}
	return nil
}

// convertArchive writes the converted archive next to dst, then renames it,
// so dst never holds a partial archive. The new archive keeps the mode of
// src.
func convertArchive(ctx context.Context, out io.Writer, src, dst, dir, store string, model message.ModelRef) ([]migrate.Result, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".harness-migrate-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	sum := md5.New()
	results, err := migrate.Archive(ctx, in, io.MultiWriter(tmp, sum), dir, store, model)
	if err == nil {
		err = tmp.Chmod(info.Mode().Perm())
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		return nil, err
	}
	written, err := os.Stat(dst)
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintf(out, "archive %s: size_bytes %d checksum_md5_base64 %s\n", dst, written.Size(), base64.StdEncoding.EncodeToString(sum.Sum(nil)))
	return results, err
}
