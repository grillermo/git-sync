// Package running records which sync operations are in flight right now:
// one marker file per operation under ~/.gitsync/running/, so `git-sync
// status` can say "syncing" while it happens. The activity log records only
// how an operation ended.
//
// One flat file per operation, created and removed atomically, so there is
// no lock: the same reasoning as pending/. A process that dies without
// cleaning up leaves its file behind, and List drops it once its pid is gone.
package running

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
)

// Marker is one in-flight operation. Peer is the machine it talks to, when
// there is one (notify: the peer told; receive: the machine that asked).
type Marker struct {
	Repo    string      `json:"repo"`
	Op      activity.Op `json:"op"`
	Peer    string      `json:"peer,omitempty"`
	Started time.Time   `json:"started"`
	PID     int         `json:"pid"`
}

// Start records an operation and returns the function that ends it; call it
// with defer. Failing to record is silent: a missing marker costs a status
// display, never a sync.
func Start(rel string, op activity.Op, peer string) (stop func()) {
	m := Marker{Repo: rel, Op: op, Peer: peer, Started: time.Now(), PID: os.Getpid()}
	if err := os.MkdirAll(config.RunningDir(), 0o755); err != nil {
		return func() {}
	}
	// The pid leads the name so List can test liveness without reading the
	// file; CreateTemp's random suffix keeps concurrent operations of one
	// process (notify to several peers) apart.
	f, err := os.CreateTemp(config.RunningDir(), fmt.Sprintf("%d-*.json", m.PID))
	if err != nil {
		return func() {}
	}
	_ = json.NewEncoder(f).Encode(m)
	f.Close()
	name := f.Name()
	var once sync.Once
	return func() { once.Do(func() { _ = os.Remove(name) }) }
}

// List returns the operations in flight, oldest first. It removes the
// markers of processes that no longer exist and skips any it cannot read
// (one being written right now reads back torn).
func List() []Marker {
	dir := config.RunningDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Marker
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		pid, err := strconv.Atoi(strings.SplitN(e.Name(), "-", 2)[0])
		if err != nil {
			continue
		}
		if !alive(pid) {
			_ = os.Remove(path)
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m Marker
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// alive is kill(pid, 0): no signal is sent, only the existence check. EPERM
// means it exists but belongs to someone else, which still counts.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
