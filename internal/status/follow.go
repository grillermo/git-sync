package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/syncer"
)

// Collect reads this machine's state and builds a snapshot. It never fails:
// a problem reading is reported in Snapshot.Error, for the app to show.
func Collect(now time.Time) Snapshot {
	cfg, err := config.Load()
	if err != nil {
		return Snapshot{At: sec(now), Repos: []Repo{}, Pending: []Pending{}, Error: err.Error()}
	}
	events, err := activity.Read()
	snap := Build(Input{Config: cfg, Events: events, Running: running.List(), Pending: syncer.ListAllPending(), Now: now})
	if err != nil {
		snap.Error = "reading the activity log: " + err.Error()
	}
	return snap
}

// Watched is every path whose change can change a snapshot.
func Watched() []string {
	return []string{config.ActivityPath(), config.PendingDir(), config.RunningDir(), config.Path()}
}

// Fingerprint summarises the size and mtime of every file and directory
// under paths. It changes whenever one is written, added or removed. A
// missing path is part of the fingerprint too, so its appearing counts.
func Fingerprint(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				fmt.Fprintf(&b, "%s -\n", path)
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			fmt.Fprintf(&b, "%s %d %d\n", path, info.Size(), info.ModTime().UnixNano())
			return nil
		})
	}
	return b.String()
}

// FollowOptions makes Follow testable without a filesystem or a clock.
type FollowOptions struct {
	Collect     func() Snapshot
	Fingerprint func() string
	Tick        time.Duration // how often to look for a change
	Refresh     time.Duration // print anyway this often, so relative times and dead markers update
}

// Follow writes a snapshot as one JSON line now, and again whenever the
// fingerprint changes or Refresh passes, until ctx ends (returns nil) or a
// write fails (the reader is gone; returns the error). Polling, rather than
// a file-notification library, keeps git-sync free of new dependencies and
// costs one stat per watched entry per tick.
func Follow(ctx context.Context, w io.Writer, o FollowOptions) error {
	emit := func() error {
		b, err := json.Marshal(o.Collect())
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	}
	last := o.Fingerprint()
	if err := emit(); err != nil {
		return err
	}
	sent := time.Now()
	t := time.NewTicker(o.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			fp := o.Fingerprint()
			if fp == last && time.Since(sent) < o.Refresh {
				continue
			}
			last = fp
			if err := emit(); err != nil {
				return err
			}
			sent = time.Now()
		}
	}
}

// WriteText is `git-sync status` without --json: the same data for a human.
func WriteText(w io.Writer, s Snapshot) {
	if s.Error != "" {
		fmt.Fprintln(w, "git-sync:", s.Error)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tSTATE\tLAST SYNC\tDETAIL")
	for _, r := range s.Repos {
		last := "-"
		if r.LastSync != nil {
			last = r.LastSync.Local().Format("2006-01-02 15:04")
		}
		detail := ""
		switch {
		case len(r.Problems) > 0:
			detail = r.Problems[0].Msg
		case len(r.Running) > 0:
			detail = string(r.Running[0].Op) + " in progress"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Repo, r.State, last, detail)
	}
	tw.Flush()
	if len(s.Pending) > 0 {
		fmt.Fprintln(w, "\nPENDING")
		for _, p := range s.Pending {
			to := "remote"
			if p.Kind == "notify" {
				to = p.Peer
			}
			fmt.Fprintf(w, "  %s -> %s  since %s\n", p.Repo, to, p.Since.Local().Format("2006-01-02 15:04"))
		}
	}
}
