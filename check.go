package harness

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// CheckName reports a session ID or blob key that a Store must refuse.
func CheckName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("harness: invalid name %q", name)
	}
	return nil
}

// CheckRecords reports an empty record, or a record with a newline.
func CheckRecords(records [][]byte) error {
	for _, r := range records {
		if len(r) == 0 || bytes.IndexByte(r, '\n') >= 0 {
			return errors.New("harness: record is empty or contains a newline")
		}
	}
	return nil
}
