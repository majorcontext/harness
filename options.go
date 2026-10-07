package harness

import "fmt"

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
