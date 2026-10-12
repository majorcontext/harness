package main

import (
	"cmp"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/httpsync"
)

// replication builds the Owner and the Sync that the config keys owner_epoch
// and sync name. Without sync it returns a nil Sync.
func replication(cfg *config.Config) (harness.Owner, harness.Sync) {
	owner := harness.NewLocalOwner(uint64(cmp.Or(cfg.OwnerEpoch, 1)))
	if cfg.Sync == nil {
		return owner, nil
	}
	return owner, httpsync.New(*cfg.Sync)
}
