package harness

import "github.com/majorcontext/harness/config"

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
