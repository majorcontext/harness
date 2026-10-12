package store

import (
	"github.com/majorcontext/harness/internal/storecheck"
)

// CheckName reports a session ID or blob key that a Store must refuse.
func CheckName(name string) error {
	return storecheck.Name(name)
}

// CheckRecords reports an empty record, or a record with a newline.
func CheckRecords(records [][]byte) error {
	return storecheck.Records(records)
}
