package syncer

import "time"

// SetRefRaceBackoff shortens the wait between ref-race retries for a test and
// returns a func restoring it.
func SetRefRaceBackoff(d time.Duration) func() {
	old := refRaceBackoff
	refRaceBackoff = d
	return func() { refRaceBackoff = old }
}
