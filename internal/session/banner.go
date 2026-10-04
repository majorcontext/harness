package session

// bannerAt returns the place of the banner in a history of n messages: after
// the newest message at the first request, and under a shorter history after a compaction.
func (a *Actor) bannerAt(n int) int {
	if !a.bannered {
		a.bannered, a.bannerPin = true, n
	}
	a.bannerPin = min(a.bannerPin, n)
	return a.bannerPin
}
