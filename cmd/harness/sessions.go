package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

// sessionRow is a session of `harness sessions --json`.
type sessionRow struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Messages  uint64    `json:"messages"`
}

func sessionsCmd(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "emit the session list as a JSON array")
	if err := fs.Parse(args); err != nil {
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
	store := harness.NewDiskStore(dir)
	rt, err := harness.New(harness.Options{Store: store, Config: *cfg, Version: version})
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
		fmt.Fprintf(&b, "%s\t%s\t%d\n", r.ID, r.CreatedAt.Format(time.RFC3339), r.Messages)
	}
	fmt.Print(b.String())
	return nil
}

// listSessions returns each session of the store in creation order, with the
// number of its messages. An empty store returns an empty list.
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
			rows = append(rows, sessionRow{ID: s.ID, CreatedAt: s.CreatedAt, Messages: msgs.Total})
		}
		if page.Next == "" {
			return rows, nil
		}
		after = page.Next
	}
}
