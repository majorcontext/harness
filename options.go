package harness

import (
	"fmt"

	"github.com/majorcontext/harness/config"
)

// ignoredKey returns the first engine-only key that c sets, or "". A Runtime
// reads none of them, so a set key would do nothing without a sign.
// session_dir stays for cmd/harness.
func ignoredKey(c config.Config) string {
	switch {
	case c.InstructionsMode != "":
		return "instructions_mode"
	case c.EventSink != nil:
		return "event_sink"
	case c.SnapshotEveryRecords != nil:
		return "snapshot_every_records"
	case c.ToolResultInlineBytes != nil:
		return "tool_result_inline_bytes"
	case c.ToolResultRetainedBytes != nil:
		return "tool_result_retained_bytes"
	}
	return ""
}

// checkEmbedder refuses a second source for what the config sets: an Owner
// beside owner_epoch, a Sync beside the sync block, a token with no URL.
func checkEmbedder(opts Options) error {
	switch {
	case opts.Owner != nil && opts.Config.OwnerEpoch != 0:
		return fmt.Errorf("%w: Options.Owner and config key owner_epoch both set the epoch", ErrInvalidRequest)
	case opts.RunToken != "" && opts.ServeURL == "":
		return fmt.Errorf("%w: Options.RunToken needs Options.ServeURL", ErrInvalidRequest)
	case opts.Sync != nil && opts.Config.Sync != nil:
		return fmt.Errorf("%w: Options.Sync and config key sync both set the receiver", ErrInvalidRequest)
	}
	return nil
}
