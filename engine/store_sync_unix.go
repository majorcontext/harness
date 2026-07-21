//go:build unix

package engine

import "os"

// syncDir fsyncs the directory at path so a newly created file's directory
// entry is durably committed (see ensureLog's fresh-file branch — a file
// fsync alone does not commit the entry linking name to inode, and the
// durable-enqueue attestation in queue.go's EnqueuePromptDurable rides on
// it).
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
