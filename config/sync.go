package config

import (
	"errors"
	"fmt"
	"net/url"
)

// SyncSpec names the receiver of a box harness: the URL that gets each SyncBatch,
// and the file that holds the bearer token.
type SyncSpec struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

func validateSync(epoch int, s *SyncSpec) error {
	if epoch < 0 {
		return fmt.Errorf("owner_epoch must not be negative, got %d", epoch)
	}
	if s == nil {
		return nil
	}
	if s.TokenFile == "" {
		return errors.New("sync: token_file is required")
	}
	u, err := url.Parse(s.URL)
	switch {
	case err != nil:
		return errors.New("sync: url is not valid")
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("sync: url must use http or https (got scheme %q)", u.Scheme)
	case u.Host == "":
		return errors.New("sync: url host is required")
	case u.User != nil:
		return errors.New("sync: url must not include userinfo")
	}
	return nil
}
