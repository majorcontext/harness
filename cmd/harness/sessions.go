package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/httpsync"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// sessionRow is a session of `harness sessions --json`.
type sessionRow struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Messages  uint64    `json:"messages"`
	Title     string    `json:"title"`
}

func sessionsCmd(args []string) error {
	if len(args) > 0 && args[0] == "import" {
		return sessionsImportCmd(args[1:], os.Stdin, os.Stdout)
	}
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "emit the session list as a JSON array")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dir, err := sessionDir(cfg.SessionDir)
	if err != nil {
		return err
	}
	store := store.NewDiskStore(dir)
	rt, err := harness.New(harness.Options{Store: store, Config: *cfg, Version: version, Getenv: os.Getenv})
	if err != nil {
		return err
	}
	rows, err := listSessions(context.Background(), rt, store)
	if err != nil {
		return err
	}
	if jsonOut {
		out, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\n", r.ID, r.CreatedAt.Format(time.RFC3339), r.Messages, r.Title)
	}
	fmt.Print(b.String())
	return nil
}

// listSessions returns each session of the store in creation order, with the
// number of its messages and its title. An empty store returns an empty list.
func listSessions(ctx context.Context, rt *harness.Runtime, store harness.Store) ([]sessionRow, error) {
	rows := []sessionRow{}
	for after := ""; ; {
		page, err := rt.List(ctx, protocol.ListSessions{After: after})
		if err != nil {
			return nil, err
		}
		for _, s := range page.Sessions {
			v, err := harness.OpenView(ctx, store, s.ID)
			if err != nil {
				return nil, err
			}
			msgs, err := v.Messages(ctx, 0, 1)
			if err != nil {
				return nil, err
			}
			rows = append(rows, sessionRow{ID: s.ID, CreatedAt: s.CreatedAt, Messages: msgs.Total, Title: s.Title})
		}
		if page.Next == "" {
			return rows, nil
		}
		after = page.Next
	}
}

// sessionsImportCmd fills an empty session directory from the SyncBatch lines of in.
func sessionsImportCmd(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("sessions import", flag.ContinueOnError)
	var dir string
	fs.StringVar(&dir, "dir", "", "the empty session directory (default: the directory of serve)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if dir == "" {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if dir, err = sessionDir(cfg.SessionDir); err != nil {
			return err
		}
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("sessions import: %s is not empty", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sum, err := httpsync.ImportBatches(context.Background(), store.NewDiskStore(dir), in)
	if err != nil {
		return fmt.Errorf("sessions import: %w", err)
	}
	_, err = fmt.Fprintf(out, "imported %d sessions, %d records, %d blobs into %s\n", sum.Sessions, sum.Records, sum.Blobs, dir)
	return err
}
