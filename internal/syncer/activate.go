package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/lock"
)

// A receive that moves a repo forward only records that the repo needs its
// ./activate run. A separate drainer runs the record later, one repo at a time
// per machine: a wake-up announce can land a dozen repos at once, and a dozen
// parallel Go builds and service restarts would swamp the machine.
//
// Each entry is a file named after the path-escaped rel, holding the HEAD
// the repo had before the first sync not yet activated. Entries are created
// with link(2), which fails if the name exists, so re-queueing a queued repo
// is a no-op that keeps the older rev - three quick commits, one build.

// QueuedActivate is one repo waiting for its ./activate.
type QueuedActivate struct {
	Rel    string
	OldRev string
	At     time.Time
}

func activateQueueDir() string { return filepath.Join(config.ActivateDir(), "queue") }

// queueName escapes rel into one flat file name. A leading dot is escaped
// too: dot-files in the queue directory are in-flight temp files.
func queueName(rel string) string {
	name := url.PathEscape(rel)
	if strings.HasPrefix(name, ".") {
		name = "%2E" + name[1:]
	}
	return name
}

// EnqueueActivate records that rel needs activating, covering every commit
// after oldRev. A no-op if rel is already queued.
func EnqueueActivate(rel, oldRev string) error {
	dir := activateQueueDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".new-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(oldRev + "\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	err = os.Link(tmp.Name(), filepath.Join(dir, queueName(rel)))
	if os.IsExist(err) {
		return nil
	}
	return err
}

// ActivateQueue lists the queued repos, oldest first.
func ActivateQueue() []QueuedActivate {
	dir := activateQueueDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var q []QueuedActivate
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rel, err := url.PathUnescape(e.Name())
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		q = append(q, QueuedActivate{Rel: rel, OldRev: strings.TrimSpace(string(b)), At: info.ModTime()})
	}
	sort.SliceStable(q, func(i, j int) bool {
		if !q[i].At.Equal(q[j].At) {
			return q[i].At.Before(q[j].At)
		}
		return q[i].Rel < q[j].Rel
	})
	return q
}

// HasActivateQueue reports whether any repo is waiting for its ./activate.
func HasActivateQueue() bool { return len(ActivateQueue()) > 0 }

// takeOldestActivate removes and returns the oldest entry. Removing before
// running is what makes a sync that lands mid-run queue the repo again.
func takeOldestActivate() (QueuedActivate, bool) {
	for _, q := range ActivateQueue() {
		if err := os.Remove(filepath.Join(activateQueueDir(), queueName(q.Rel))); err == nil {
			return q, true
		}
	}
	return QueuedActivate{}, false
}

// dropQueuedActivate forgets rel's entry, if any.
func dropQueuedActivate(rel string) {
	_ = os.Remove(filepath.Join(activateQueueDir(), queueName(rel)))
}

// emptyTree is git's well-known empty tree. A manual run passes it as
// GITSYNC_OLD_REV so `git diff OLD NEW` reports every file as changed.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// drainLock names the machine-wide lock that keeps activates serial. The
// leading dot keeps it out of the way of any real repo's lock.
const drainLock = ".activate"

// ActivateOwner is the Owner.From a repo lock carries while its ./activate
// runs, so the commit hooks can say so.
const ActivateOwner = "./activate"

// ActivateLogPath is where rel's ./activate output is kept.
func ActivateLogPath(rel string) string {
	return filepath.Join(config.ActivateDir(), queueName(rel)+".log")
}

// activateScript returns dir/activate if it is an executable regular file.
func activateScript(dir string) (string, bool) {
	p := filepath.Join(dir, "activate")
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return p, true
}

// activateTimeout bounds one run. GITSYNC_ACTIVATE_TIMEOUT exists for tests.
func activateTimeout() time.Duration {
	if s := os.Getenv("GITSYNC_ACTIVATE_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Minute
}

// DrainActivate is `git-sync activate --drain`: it runs every queued
// ./activate, one at a time, until the queue is empty. If another drainer
// holds the machine-wide lock it returns at once - that one will get to
// whatever was just queued.
func DrainActivate() int {
	cfg, err := config.Load()
	if err != nil {
		return 1
	}
	for {
		l, err := lock.Acquire(drainLock, 0)
		if err != nil {
			if lock.IsBusy(err) {
				return 0
			}
			activity.AppendDebug("activate: " + err.Error())
			return 1
		}
		stop := heartbeat(l)
		postponed := drainQueue(cfg)
		stop()
		l.Release()
		// An entry queued after our last look, by a sync whose own drainer
		// found the lock still held and left, would otherwise wait for the
		// next sync. A postponed repo is retried by the drainer its receive
		// starts, so do not spin on it here.
		if postponed || !HasActivateQueue() {
			return 0
		}
	}
}

// drainQueue runs entries until none are left. Reports true if it stopped
// early because a repo was busy receiving. Stopping the whole round on one
// busy repo (rather than skipping to the next) is deliberate: the busy
// repo's receive starts its own drainer when it finishes, which resumes the
// rest of the queue, and the postponed entry keeps its place by staying
// queued.
func drainQueue(cfg config.Config) (postponed bool) {
	for {
		q, ok := takeOldestActivate()
		if !ok {
			return false
		}
		if !runQueued(cfg, q) {
			return true
		}
	}
}

// runQueued runs one entry. Returns false if the repo was busy, in which
// case the entry is back in the queue.
func runQueued(cfg config.Config, q QueuedActivate) bool {
	skip := func(msg string) {
		_ = activity.Append(activity.Event{
			Repo: q.Rel, Op: activity.OpActivate, Status: activity.StatusSkip, Msg: msg,
		})
	}
	if cfg.ValidateRel(q.Rel) != nil || !cfg.IsSelected(q.Rel) {
		skip("no longer selected for syncing, dropped")
		return true
	}
	dir := cfg.RepoPath(q.Rel)
	if _, ok := activateScript(dir); !ok {
		skip("no ./activate any more, dropped")
		return true
	}

	// The repo's own receive lock: no fast-forward may rewrite the tree
	// under a build, and no build may start mid-fast-forward.
	l, err := lock.AcquireFrom(q.Rel, ActivateOwner, lockTimeout())
	if err != nil {
		_ = EnqueueActivate(q.Rel, q.OldRev)
		skip("repo busy syncing, activate postponed")
		return false
	}
	defer l.Release()
	stop := heartbeat(l)
	defer stop()

	runActivate(context.Background(), dir, q.Rel, q.OldRev, nil)
	return true
}

// ActivateNow is `git-sync activate <repo>` from a terminal: it waits for
// any running drainer, then runs rel's ./activate in the foreground with
// its output on out as well as in the log. It treats every file as changed.
// It holds the same machine-wide and per-repo locks as the drainer, so the
// two can never run (or write the log) at once.
func ActivateNow(rel string, out io.Writer) int {
	return ActivateNowContext(context.Background(), rel, out)
}

// ActivateNowContext is ActivateNow that stops the script (its whole process
// group) when ctx is cancelled, still releasing its locks on the way out.
func ActivateNowContext(ctx context.Context, rel string, out io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(out, "activate:", err)
		return 1
	}
	if err := cfg.ValidateRel(rel); err != nil {
		fmt.Fprintln(out, "activate:", err)
		return 2
	}
	if !cfg.IsSelected(rel) {
		fmt.Fprintf(out, "activate: %s is not synced on this machine\n", rel)
		return 1
	}
	dir := cfg.RepoPath(rel)
	if _, ok := activateScript(dir); !ok {
		fmt.Fprintf(out, "activate: %s has no executable ./activate\n", rel)
		return 1
	}

	g, err := acquireCtx(ctx, drainLock, "", activateTimeout()+time.Minute)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(out, "activate: interrupted")
			return exitInterrupted
		}
		fmt.Fprintln(out, "activate: another ./activate is still running:", err)
		return 1
	}
	defer g.Release()
	stopG := heartbeat(g)
	defer stopG()

	l, err := acquireCtx(ctx, rel, ActivateOwner, lockTimeout())
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(out, "activate: interrupted")
			return exitInterrupted
		}
		fmt.Fprintf(out, "activate: %s is busy syncing, try again shortly\n", rel)
		return 1
	}
	defer l.Release()
	stopL := heartbeat(l)
	defer stopL()

	dropQueuedActivate(rel) // this run covers it
	if runActivate(ctx, dir, rel, emptyTree, out) {
		return 0
	}
	return 1
}

// exitInterrupted is the conventional status for a run ended by SIGINT.
const exitInterrupted = 130

// acquireCtx is lock.AcquireFrom that gives up as soon as ctx is done, so a
// signal trapped by the caller can still end a long wait. It polls with
// zero-timeout attempts, which keeps the lock package's stale reclaim.
func acquireCtx(ctx context.Context, rel, from string, timeout time.Duration) (*lock.Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l, err := lock.AcquireFrom(rel, from, 0)
		if err == nil || !lock.IsBusy(err) || time.Now().After(deadline) {
			return l, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// runActivate runs dir/activate for rel, logging its output to
// ActivateLogPath (and also to tee, if given) and the outcome to the
// activity log. Reports whether it succeeded.
func runActivate(parent context.Context, dir, rel, oldRev string, tee io.Writer) bool {
	script, _ := activateScript(dir)
	newRev, _ := gitcmd.Run(dir, "rev-parse", "HEAD")
	logPath := ActivateLogPath(rel)
	event := func(s activity.Status, msg string) {
		_ = activity.Append(activity.Event{Repo: rel, Op: activity.OpActivate, Status: s, Msg: msg})
	}

	logf, err := openActivateLog(logPath)
	if err != nil {
		event(activity.StatusError, "could not open "+logPath+": "+err.Error())
		return false
	}
	defer logf.Close()
	fmt.Fprintf(logf, "==> %s %s..%s\n", time.Now().Format(time.RFC3339), shortRev(oldRev), shortRev(newRev))

	// A plain file as stdout is handed to the child as is; only a tee needs
	// a pipe, and WaitDelay stops a grandchild holding that pipe open from
	// hanging us.
	var out io.Writer = logf
	if tee != nil {
		out = io.MultiWriter(logf, tee)
	}
	ctx, cancel := context.WithTimeout(parent, activateTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, script)
	cmd.Dir = dir
	cmd.Env = activateEnv(oldRev, newRev)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = groupAttr()
	cmd.Cancel = func() error { return killGroup(cmd.Process) }
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	err = cmd.Run()
	took := time.Since(start).Round(100 * time.Millisecond)
	fmt.Fprintf(logf, "<== %v after %s\n\n", errOrOK(err), took)

	// say tells a terminal user why the run ended badly; the drainer has no
	// terminal and relies on the activity log alone.
	say := func(msg string) {
		if tee != nil {
			fmt.Fprintln(tee, msg)
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		msg := fmt.Sprintf("./activate timed out after %s, see %s", took, logPath)
		event(activity.StatusError, msg)
		say("activate: " + msg)
		return false
	case parent.Err() != nil && err != nil:
		msg := fmt.Sprintf("./activate interrupted after %s, see %s", took, logPath)
		event(activity.StatusError, msg)
		say("activate: " + msg)
		return false
	case err != nil:
		msg := fmt.Sprintf("./activate failed (%v) after %s, see %s", err, took, logPath)
		event(activity.StatusError, msg)
		say("activate: " + msg)
		return false
	}
	event(activity.StatusOK, fmt.Sprintf("activated %s..%s in %s", shortRev(oldRev), shortRev(newRev), took))
	return true
}

// activateEnv is this process's environment for user code: minus git-sync's
// internal marker, so a commit or push ./activate attempts is treated as a
// real one and refused by pre-commit/pre-push while the repo lock is held
// for the run (intended), plus the rev range.
func activateEnv(oldRev, newRev string) []string {
	var env []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "GITSYNC_INTERNAL="),
			strings.HasPrefix(kv, "GITSYNC_OLD_REV="),
			strings.HasPrefix(kv, "GITSYNC_NEW_REV="):
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GITSYNC_OLD_REV="+oldRev, "GITSYNC_NEW_REV="+newRev)
}

// openActivateLog opens path for appending, first rotating it to path.1
// once it passes 256 KiB, so a chatty build cannot grow it forever.
func openActivateLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 256<<10 {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

func shortRev(rev string) string {
	switch {
	case rev == emptyTree:
		return "(everything)"
	case len(rev) > 7:
		return rev[:7]
	}
	return rev
}

func errOrOK(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// queueActivateIfMoved queues rel's ./activate if it has one and HEAD has
// moved off before. Only queues: the command layer starts the drainer once
// the receive lock is released.
func queueActivateIfMoved(rel, dir, before string) {
	if _, ok := activateScript(dir); !ok {
		return
	}
	after, err := gitcmd.Run(dir, "rev-parse", "HEAD")
	if err != nil || before == "" || after == before {
		return
	}
	if err := EnqueueActivate(rel, before); err != nil {
		activity.AppendDebug("activate: could not queue " + rel + ": " + err.Error())
	}
}
