package activity

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/grillermo/git-sync/internal/config"
)

// MaxLineLen keeps a serialised event under the POSIX PIPE_BUF guarantee, so
// a single O_APPEND write from any number of concurrent processes lands
// whole and never interleaves with another. This is why there is no lock
// around the log: several pushes and receives append to it at once.
const MaxLineLen = 4096

// Append writes one event. Errors are returned but callers generally ignore
// them: failing to log must never fail a sync.
func Append(e Event) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	line, err := marshalBounded(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(config.ActivityPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line) // one Write call, or the atomicity guarantee is void
	return err
}

// marshalBounded serialises e, shrinking Msg until the line fits MaxLineLen.
func marshalBounded(e Event) ([]byte, error) {
	for {
		b, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		if len(b)+1 <= MaxLineLen {
			return append(b, '\n'), nil
		}
		over := len(b) + 1 - MaxLineLen
		if len(e.Msg) <= over+3 {
			// Nothing left to trim; drop the message entirely.
			e.Msg = ""
			b, err = json.Marshal(e)
			if err != nil {
				return nil, err
			}
			return append(b, '\n'), nil
		}
		e.Msg = e.Msg[:len(e.Msg)-over-3] + "..."
	}
}

// Read returns every event, oldest first. A missing log is not an error: a
// fresh install has simply not synced anything yet. Corrupt lines - a torn
// write from a killed process - are skipped rather than failing the read,
// because a broken line must not cost you your whole history.
func Read() ([]Event, error) {
	f, err := os.Open(config.ActivityPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineLen*2)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events, sc.Err()
}

// AppendDebug records raw git/ssh output for troubleshooting. Separate from
// activity.jsonl so the report's data source stays clean and parseable.
func AppendDebug(s string) {
	_ = os.MkdirAll(config.Home(), 0o755)
	f, err := os.OpenFile(config.DebugLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().Format(time.RFC3339) + " " + s + "\n")
}
