# ./activate (git-sync side) Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After a receive fast-forwards a repo that has an executable `./activate`, git-sync queues it and runs the queue one script at a time on that machine.

**Architecture:** A file-per-repo queue under `~/.gitsync/activate/queue/` (atomic create-if-absent, so a repo is queued at most once and keeps the oldest `OLD_REV`). `receive` only enqueues. A detached `git-sync activate --drain` takes one machine-wide lock, pops entries oldest-first, and runs each `./activate` under that repo's receive lock with output going to a per-repo log. The command layer starts the drainer after `receive`/`announce`/`watch`, never the `syncer` package itself, so unit tests never launch a real process.

**Tech Stack:** Go 1.26, stdlib only (`os/exec`, `net/url`, `syscall`), existing `internal/lock`, `internal/activity`, `internal/gitcmd`, `internal/testutil`.

**Spec:** `docs/superpowers/specs/2026-10-09-activate-design.md`

**Starting from a fresh session:** read the spec first, then this plan. This is plan 1 of 2. Plan 2, `docs/superpowers/plans/2026-10-09-activate-repos.md`, writes the per-repo scripts and depends on this one being finished and rolled out. Ticked checkboxes record progress. As of commit `4dbfd85` nothing is implemented. Tick each step as you finish it and commit the plan along with the code, so the next session knows where to resume. Decisions already made with the user, not to re-open:
- Only receiving machines run `./activate`, never the committing machine.
- Runs are serial per machine, through the queue.
- Repos used by other people (`comunidad-antesis`, `readitsoon`, `server`) never get an `activate`.
- The five personal services' `activate` only delegates to `./serve`.
- Open shells only get a notice; nothing re-sources automatically.
- `readitsoon-companion` is out of scope.

**Deliberate differences from the spec (YAGNI):**
- No `GITSYNC_FROM`: a coalesced entry can cover commits from several machines, so there is no single honest value.
- `initialsync` does not enqueue. Install is a one-off. Run `git-sync activate <repo>` by hand afterwards if needed.
- Manual runs (`git-sync activate <repo>`) pass git's empty-tree hash as `GITSYNC_OLD_REV`, so a script's `git diff OLD NEW -- paths` sees every file as changed and does the full job.

---

## File map

| File | Responsibility |
|---|---|
| `internal/config/config.go` | + `ActivateDir()` path helper |
| `internal/activity/event.go` | + `OpActivate` |
| `internal/syncer/activate.go` (new) | queue, drainer, running one script, manual run |
| `internal/syncer/detach_unix.go` | + process-group helpers so a timeout kills the whole script tree |
| `internal/syncer/receive.go` | enqueue after a fast-forward that moved HEAD |
| `internal/syncer/hook.go` | `SpawnActivateDrain`; `Block` names a running `./activate` |
| `internal/syncer/watch.go` | `WatchOptions.AfterAnnounce` |
| `cmd/git-sync/main.go`, `cmd/git-sync/stubs.go` | `activate` subcommand, kick the drainer after receive/announce/watch, `currentRel` helper shared with `unlock` |
| `internal/syncer/activate_test.go` (new) | queue, drain, serial, manual tests |
| `internal/syncer/receive_test.go`, `hook_test.go`, `watch_test.go`, `e2e_test.go` | integration tests |
| `CLAUDE.md`, `README.md` | document the feature |

Run everything from `/Users/grillermo/c/git-sync`. `NewSandbox` tests must never use `t.Parallel()`.

---

## Chunk 1: Queue

### Task 1: Paths, op, and the queue

**Files:**
- Modify: `internal/config/config.go:94` (after `PendingDir`)
- Modify: `internal/activity/event.go:14`
- Create: `internal/syncer/activate.go`
- Create: `internal/syncer/activate_test.go`

- [x] **Step 1: Write the failing tests**

`internal/syncer/activate_test.go`:

```go
package syncer_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

// writeActivate puts an executable ./activate running body into repoDir.
func writeActivate(t *testing.T, repoDir, body string) {
	t.Helper()
	p := filepath.Join(repoDir, "activate")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// waitForFile polls until path exists or timeout passes.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %v", path, timeout)
}

func TestEnqueueActivateKeepsTheOldestRev(t *testing.T) {
	testutil.NewSandbox(t)
	if err := syncer.EnqueueActivate("group/proj", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := syncer.EnqueueActivate("group/proj", "bbb"); err != nil {
		t.Fatal(err)
	}
	q := syncer.ActivateQueue()
	if len(q) != 1 || q[0].Rel != "group/proj" || q[0].OldRev != "aaa" {
		t.Fatalf("queue = %+v, want one group/proj entry from aaa", q)
	}
}

func TestActivateQueueIsOldestFirstAndRoundTripsNames(t *testing.T) {
	testutil.NewSandbox(t)
	want := []string{"b", "group/a", ".dotfiles"}
	for _, rel := range want {
		if err := syncer.EnqueueActivate(rel, "r"); err != nil {
			t.Fatal(err)
		}
		// Order is by mtime; keep entries apart on coarse filesystems.
		time.Sleep(20 * time.Millisecond)
	}
	q := syncer.ActivateQueue()
	if len(q) != len(want) {
		t.Fatalf("queue = %+v, want %d entries", q, len(want))
	}
	for i, rel := range want {
		if q[i].Rel != rel {
			t.Errorf("queue[%d] = %q, want %q", i, q[i].Rel, rel)
		}
	}
}

func TestHasActivateQueue(t *testing.T) {
	testutil.NewSandbox(t)
	if syncer.HasActivateQueue() {
		t.Fatal("empty queue reported as non-empty")
	}
	_ = syncer.EnqueueActivate("group/proj", "r")
	if !syncer.HasActivateQueue() {
		t.Fatal("queued entry not reported")
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test -run 'Activate' ./internal/syncer/`
Expected: FAIL, `undefined: syncer.EnqueueActivate` (build error).

- [x] **Step 3: Implement**

`internal/config/config.go`, after `PendingDir`:

```go
func ActivateDir() string   { return filepath.Join(Home(), "activate") }
```

(Run `gofmt -w internal/config` afterwards. It realigns the block.)

`internal/activity/event.go`, add to the `Op` constants:

```go
	OpActivate Op = "activate" // running a repo's ./activate after a sync
```

`internal/syncer/activate.go`:

```go
package syncer

import (
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/config"
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
```

- [x] **Step 4: Run to verify pass**

Run: `go test -run 'Activate' ./internal/syncer/ && go vet ./...`
Expected: PASS. `takeOldestActivate`/`dropQueuedActivate` are unused until Task 2. `go vet` doesn't flag unused functions, but if a linter complains, carry on.

- [x] **Step 5: Commit**

```bash
gofmt -w internal/config internal/activity internal/syncer
git add internal/config/config.go internal/activity/event.go internal/syncer/activate.go internal/syncer/activate_test.go
git commit -m "feat(activate): add the per-machine activate queue"
```

---

## Chunk 2: Running activate

### Task 2: Drain the queue and run one script

**Files:**
- Modify: `internal/syncer/activate.go`
- Modify: `internal/syncer/detach_unix.go`
- Test: `internal/syncer/activate_test.go`

- [x] **Step 1: Write the failing tests** (append to `activate_test.go`; add imports `strings`, `github.com/grillermo/git-sync/internal/activity`)

```go
func TestDrainRunsActivateWithTheRevRange(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `echo "old=$GITSYNC_OLD_REV new=$GITSYNC_NEW_REV internal=$GITSYNC_INTERNAL." > "$HOME/ran"`)
	// git-sync's own marker must never reach user code.
	t.Setenv("GITSYNC_INTERNAL", "1")
	head := strings.TrimSpace(sb.Git(repo, "rev-parse", "HEAD"))
	if err := syncer.EnqueueActivate("group/proj", "abc123"); err != nil {
		t.Fatal(err)
	}

	if code := syncer.DrainActivate(); code != 0 {
		t.Fatalf("DrainActivate = %d, want 0", code)
	}
	testutil.AssertFileContains(t, filepath.Join(sb.Home, "ran"), "old=abc123 new="+head+" internal=.")
	if syncer.HasActivateQueue() {
		t.Error("queue should be empty after a drain")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusOK, "activated")
}

func TestDrainRecordsAFailingActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `echo boom-output; exit 3`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	if code := syncer.DrainActivate(); code != 0 {
		t.Fatalf("DrainActivate = %d, want 0 (a failed activate is an event, not a drain failure)", code)
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusError, "failed")
	testutil.AssertFileContains(t, syncer.ActivateLogPath("group/proj"), "boom-output")
}

func TestDrainKillsAnActivateThatRunsTooLong(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	t.Setenv("GITSYNC_ACTIVATE_TIMEOUT", "300ms")
	// The child sleep is what must die too, not just the shell.
	writeActivate(t, repo, `sleep 5 & wait`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	start := time.Now()
	syncer.DrainActivate()
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("drain took %v; the timeout did not stop the script", took)
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusError, "timed out")
}

func TestDrainDropsARepoNoLongerSelected(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	sb.MakeRepo("group/other")
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"group/other"})
	writeActivate(t, repo, `touch "$HOME/ran"`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	syncer.DrainActivate()
	if _, err := os.Stat(filepath.Join(sb.Home, "ran")); err == nil {
		t.Error("ran ./activate for a repo that is not selected")
	}
	if syncer.HasActivateQueue() {
		t.Error("entry should have been dropped")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusSkip, "dropped")
}

func TestDrainDropsARepoWithoutActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	_ = syncer.EnqueueActivate("group/proj", "r")

	syncer.DrainActivate()
	if syncer.HasActivateQueue() {
		t.Error("entry should have been dropped")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusSkip, "no ./activate")
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test -run 'Drain' ./internal/syncer/`
Expected: build error, `undefined: syncer.DrainActivate`.

- [x] **Step 3: Implement**

Append to `internal/syncer/detach_unix.go`:

```go
// groupAttr starts a child as the leader of a new process group, so
// killGroup can stop it together with everything it started.
func groupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the process group p leads.
func killGroup(p *os.Process) error {
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}
```

(and add `"os"` to its imports).

Append to `internal/syncer/activate.go` (merge imports: `context`, `errors`, `fmt`, `io`, `os/exec`, plus `activity`, `gitcmd`, `lock`):

```go
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
		if d, err := time.ParseDuration(s); err == nil {
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
// early because a repo was busy receiving.
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

	runActivate(dir, q.Rel, q.OldRev, nil)
	return true
}

// runActivate runs dir/activate for rel, logging its output to
// ActivateLogPath (and also to tee, if given) and the outcome to the
// activity log. Reports whether it succeeded.
func runActivate(dir, rel, oldRev string, tee io.Writer) bool {
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
	ctx, cancel := context.WithTimeout(context.Background(), activateTimeout())
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

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		event(activity.StatusError, fmt.Sprintf("./activate timed out after %s, see %s", took, logPath))
		return false
	case err != nil:
		event(activity.StatusError, fmt.Sprintf("./activate failed (%v) after %s, see %s", err, took, logPath))
		return false
	}
	event(activity.StatusOK, fmt.Sprintf("activated %s..%s in %s", shortRev(oldRev), shortRev(newRev), took))
	return true
}

// activateEnv is this process's environment for user code: minus git-sync's
// internal marker (a commit ./activate made would be a real commit), plus
// the rev range.
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
```

- [x] **Step 4: Run to verify pass**

Run: `go test -race -run 'Activate|Drain' ./internal/syncer/`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
gofmt -w internal/syncer
git add internal/syncer/activate.go internal/syncer/detach_unix.go internal/syncer/activate_test.go
git commit -m "feat(activate): drain the queue, one ./activate at a time"
```

### Task 3: Serial execution, re-queue mid-run, busy repo

These pin the behaviour Task 2's code already implements. If one fails, fix the code, not the test.

**Files:**
- Test: `internal/syncer/activate_test.go` (add import `github.com/grillermo/git-sync/internal/lock`, `sync`)

- [x] **Step 1: Write the tests**

```go
func TestDrainersNeverRunTwoActivatesAtOnce(t *testing.T) {
	sb := testutil.NewSandbox(t)
	for _, rel := range []string{"a", "b", "c"} {
		dir := sb.MakeRepo(rel)
		writeActivate(t, dir, `echo start >> "$HOME/runs"; sleep 0.2; echo end >> "$HOME/runs"`)
	}
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	for _, rel := range []string{"a", "b", "c"} {
		_ = syncer.EnqueueActivate(rel, "r")
	}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); syncer.DrainActivate() }()
	}
	wg.Wait()

	b, err := os.ReadFile(filepath.Join(sb.Home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), strings.Repeat("start\nend\n", 3); got != want {
		t.Errorf("runs overlapped or went missing:\n%s\nwant:\n%s", got, want)
	}
}

func TestASyncLandingMidRunActivatesAgainAfter(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `echo run >> "$HOME/runs"; touch "$HOME/started"; sleep 0.5`)
	_ = syncer.EnqueueActivate("group/proj", "r1")

	done := make(chan int)
	go func() { done <- syncer.DrainActivate() }()
	waitForFile(t, filepath.Join(sb.Home, "started"), 5*time.Second)
	if err := syncer.EnqueueActivate("group/proj", "r2"); err != nil {
		t.Fatal(err)
	}
	<-done

	b, _ := os.ReadFile(filepath.Join(sb.Home, "runs"))
	if n := strings.Count(string(b), "run\n"); n != 2 {
		t.Errorf("./activate ran %d times, want 2 (once more for the mid-run sync)", n)
	}
}

func TestDrainPostponesARepoThatIsMidReceive(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	t.Setenv("GITSYNC_LOCK_TIMEOUT", "200ms")
	writeActivate(t, repo, `touch "$HOME/ran"`)
	l, err := lock.AcquireFrom("group/proj", "peer.example", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_ = syncer.EnqueueActivate("group/proj", "r")

	if code := syncer.DrainActivate(); code != 0 {
		t.Fatalf("DrainActivate = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(sb.Home, "ran")); err == nil {
		t.Error("ran ./activate under a receive")
	}
	if !syncer.HasActivateQueue() {
		t.Error("a postponed repo must stay queued")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusSkip, "postponed")
}
```

- [x] **Step 2: Run**

Run: `go test -race -run 'Drain|MidRun' ./internal/syncer/`
Expected: PASS.

- [x] **Step 3: Commit**

```bash
git add internal/syncer/activate_test.go
git commit -m "test(activate): pin serial drain, mid-run requeue, busy repo"
```

---

## Chunk 3: Wiring it in

### Task 4: Receive enqueues after a fast-forward that moved HEAD

**Files:**
- Modify: `internal/syncer/receive.go` (`syncRepo`, around the `FastForward` call and the stash-pop block)
- Test: `internal/syncer/receive_test.go`

- [x] **Step 1: Write the failing tests** (append to `receive_test.go`; add import `os` if missing)

```go
// peerCommitActivate commits an executable ./activate from the peer clone
// and pushes it: "the other machine added an activate step".
func peerCommitActivate(t *testing.T, sb *testutil.Sandbox, rel, body string) {
	t.Helper()
	dst := filepath.Join(sb.Home, "peer", rel)
	writeActivate(t, dst, body)
	sb.Git(dst, "add", "activate")
	sb.Git(dst, "commit", "-qm", "add activate")
	sb.Git(dst, "push", "-q")
}

func TestReceiveQueuesActivateWhenHeadMoves(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	before := strings.TrimSpace(sb.Git(repo, "rev-parse", "HEAD"))
	peerCommitActivate(t, sb, "group/proj", "true")

	if code := syncer.Receive("group/proj", "peer.example"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	q := syncer.ActivateQueue()
	if len(q) != 1 || q[0].Rel != "group/proj" || q[0].OldRev != before {
		t.Fatalf("queue = %+v, want group/proj from %s", q, before)
	}
}

func TestReceiveQueuesNothingWhenNothingArrived(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, "true")
	sb.Git(repo, "add", "activate")
	sb.Git(repo, "commit", "-qm", "add activate")
	sb.Git(repo, "push", "-q")

	syncer.Receive("group/proj", "peer.example")
	if syncer.HasActivateQueue() {
		t.Error("queued an activate though HEAD did not move")
	}
}

func TestReceiveQueuesNothingWithoutAnActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from-peer")

	syncer.Receive("group/proj", "peer.example")
	if syncer.HasActivateQueue() {
		t.Error("queued an activate for a repo that has none")
	}
}

func TestReceiveQueuesNothingWhenDiverged(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	peerCommitActivate(t, sb, "group/proj", "true")
	testutil.Commit(t, sb, repo, "divergent local commit")

	syncer.Receive("group/proj", "peer.example")
	if syncer.HasActivateQueue() {
		t.Error("queued an activate though the fast-forward failed")
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test -run 'ReceiveQueues' ./internal/syncer/`
Expected: `TestReceiveQueuesActivateWhenHeadMoves` FAILS (queue empty); the others pass.

- [x] **Step 3: Implement**

In `syncRepo` (`internal/syncer/receive.go`), replace the fast-forward `if/else` with:

```go
	before, _ := gitcmd.Run(dir, "rev-parse", "HEAD")
	fastForwarded := false
	if err := gitcmd.FastForward(dir, remote, branch); err != nil {
		// The fetch already updated the remote-tracking refs, so the user has
		// everything they need locally to merge by hand.
		log(activity.StatusWarn, branch, "diverged from "+remote+"/"+branch+
			", fetched only, manual merge needed")
	} else {
		fastForwarded = true
		log(activity.StatusOK, branch, "fast-forwarded "+branch+" from "+remote)
	}
```

and after the stash-pop block, just before `return 0`:

```go
	// After the pop: ./activate may be untracked-then-committed, and the
	// stash would have hidden it a moment ago.
	if fastForwarded {
		queueActivateIfMoved(rel, dir, before)
	}
```

Add to `activate.go`:

```go
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
```

- [x] **Step 4: Run to verify pass**

Run: `go test -race ./internal/syncer/ -run 'Receive'`
Expected: PASS (all old receive tests too).

- [x] **Step 5: Commit**

```bash
gofmt -w internal/syncer
git add internal/syncer/receive.go internal/syncer/activate.go internal/syncer/receive_test.go
git commit -m "feat(activate): queue ./activate when a receive moves HEAD"
```

### Task 5: Manual run and the commit-hook message

**Files:**
- Modify: `internal/syncer/activate.go`, `internal/syncer/hook.go` (`Block`)
- Test: `internal/syncer/activate_test.go`, `internal/syncer/hook_test.go`

- [ ] **Step 1: Write the failing tests**

`activate_test.go` (add import `bytes`):

```go
func TestActivateNowTreatsEveryFileAsChanged(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `git diff --name-only "$GITSYNC_OLD_REV" "$GITSYNC_NEW_REV"`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	var out bytes.Buffer
	if code := syncer.ActivateNow("group/proj", &out); code != 0 {
		t.Fatalf("ActivateNow = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "README.md") {
		t.Errorf("output should list every tracked file, got:\n%s", out.String())
	}
	if syncer.HasActivateQueue() {
		t.Error("a manual run covers the queued entry; it should be gone")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusOK, "everything")
}

func TestActivateNowRefusesARepoWithoutActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	var out bytes.Buffer
	if code := syncer.ActivateNow("group/proj", &out); code != 1 {
		t.Fatalf("ActivateNow = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "no executable ./activate") {
		t.Errorf("unhelpful message: %q", out.String())
	}
}
```

`hook_test.go`:

```go
func TestBlockNamesARunningActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, err := lock.AcquireFrom("group/proj", syncer.ActivateOwner, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	var out bytes.Buffer
	if code := syncer.Block(repo, &out); code != 1 {
		t.Fatalf("Block = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "running its ./activate") {
		t.Errorf("message %q should say ./activate is running", out.String())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'ActivateNow|BlockNames' ./internal/syncer/`
Expected: build error `undefined: syncer.ActivateNow`.

- [ ] **Step 3: Implement**

`activate.go`:

```go
// ActivateNow is `git-sync activate <repo>` from a terminal: it waits for
// any running drainer, then runs rel's ./activate in the foreground with
// its output on out as well as in the log. It treats every file as changed.
func ActivateNow(rel string, out io.Writer) int {
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

	g, err := lock.Acquire(drainLock, activateTimeout()+time.Minute)
	if err != nil {
		fmt.Fprintln(out, "activate: another ./activate is still running:", err)
		return 1
	}
	defer g.Release()
	stopG := heartbeat(g)
	defer stopG()

	l, err := lock.AcquireFrom(rel, ActivateOwner, lockTimeout())
	if err != nil {
		fmt.Fprintf(out, "activate: %s is busy syncing, try again shortly\n", rel)
		return 1
	}
	defer l.Release()
	stopL := heartbeat(l)
	defer stopL()

	dropQueuedActivate(rel) // this run covers it
	if runActivate(dir, rel, emptyTree, out) {
		return 0
	}
	return 1
}
```

`hook.go`, in `Block`, replace the two `Fprintf` lines with:

```go
	if owner.From == ActivateOwner {
		fmt.Fprintf(w, "git-sync: %s is running its ./activate (started %s ago).\n",
			rel, owner.Age().Round(time.Second))
	} else {
		fmt.Fprintf(w, "git-sync: %s is receiving changes from %s (started %s ago).\n",
			rel, from, owner.Age().Round(time.Second))
	}
	fmt.Fprintf(w, "Wait a moment and try again. If this is stuck: git-sync unlock %s\n", rel)
```

- [ ] **Step 4: Run to verify pass**

Run: `go test -race ./internal/syncer/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/syncer
git add internal/syncer
git commit -m "feat(activate): run ./activate by hand; name it in the commit hook"
```

### Task 6: Start the drainer: receive, announce, watch, CLI

**Files:**
- Modify: `internal/syncer/hook.go` (next to `SpawnRetry`)
- Modify: `internal/syncer/watch.go` (`WatchOptions`, `Watch`, `defaults`)
- Modify: `cmd/git-sync/main.go` (usage + dispatch), `cmd/git-sync/stubs.go` (`cmdReceive`, `cmdAnnounce`, `cmdWatch`, `cmdUnlock`, new `cmdActivate`)
- Test: `internal/syncer/watch_test.go`

- [ ] **Step 1: Write the failing watch test**

```go
func TestWatchRunsAfterAnnounceAfterEveryAnnounce(t *testing.T) {
	c := &fakeClock{now: time.Unix(1e9, 0), asleep: []time.Duration{0, 8 * time.Hour, 0}}
	var order []string
	syncer.Watch(syncer.WatchOptions{
		Interval: 15 * time.Second, Slack: time.Minute,
		BinPath: fakeBin(t), StillWanted: wantedFor(c, 3),
		Now: c.Now, Sleep: c.Sleep,
		Announce:      func(<-chan struct{}) { order = append(order, "announce") },
		AfterAnnounce: func() { order = append(order, "after") },
	})
	if got := strings.Join(order, ","); got != "announce,after,announce,after" {
		t.Errorf("order = %s, want announce,after twice", got)
	}
}
```

(add `strings` to the imports).

- [ ] **Step 2: Run to verify failure**

Run: `go test -run TestWatchRunsAfterAnnounce ./internal/syncer/`
Expected: build error `unknown field AfterAnnounce`.

- [ ] **Step 3: Implement**

`watch.go`: add to `WatchOptions` below `Announce`:

```go
	// AfterAnnounce runs after every announce. The command layer uses it to
	// start the activate drainer, which also picks up anything a crashed
	// drainer left queued.
	AfterAnnounce func()
```

In `Watch`, after **both** `o.Announce(o.Stop)` calls add `o.AfterAnnounce()`. In `defaults`:

```go
	if o.AfterAnnounce == nil {
		o.AfterAnnounce = func() {}
	}
```

`hook.go`, next to `SpawnRetry`:

```go
// SpawnActivateDrain starts `self activate --drain` detached, for the same
// reason SpawnRetry is detached: receive runs under a peer's ssh session.
func SpawnActivateDrain(self string) error {
	return spawnDetached(self, "activate", "--drain")
}
```

`stubs.go`:

1. Add a helper below `cmdRetry` and use it in `cmdReceive`, `cmdAnnounce`, `cmdWatch`:

```go
// kickActivate starts the activate drainer if anything is queued. Called
// only once the receive lock is released, so the drainer can take it.
func kickActivate() {
	if !syncer.HasActivateQueue() {
		return
	}
	if self, err := os.Executable(); err == nil {
		_ = syncer.SpawnActivateDrain(self)
	}
}
```

- `cmdReceive`: call `kickActivate()` right after `code := syncer.Receive(repo, *from)`.
- `cmdAnnounce`: `code := syncer.Announce(); kickActivate(); return code`.
- `cmdWatch`: add `AfterAnnounce: kickActivate,` to the `WatchOptions` literal.

2. Pull the "repo from cwd" block out of `cmdUnlock` into:

```go
// currentRel is the selected-repo relpath of the repo containing the
// working directory, for commands whose <repo> argument is optional.
func currentRel(cfg config.Config) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := gitcmd.Toplevel(wd)
	if err != nil {
		return "", errors.New("not inside a git repo; name one")
	}
	// git resolves symlinks in --show-toplevel (e.g. macOS's /var ->
	// /private/var), but base_dir as configured is not resolved.
	// Resolve here too, or a repo under a symlinked ancestor looks
	// "outside base_dir". Mirrors syncer's repoRel.
	rc := cfg
	if resolved, err := filepath.EvalSymlinks(rc.BaseDir); err == nil {
		rc.BaseDir = resolved
	}
	return rc.RepoRel(root)
}
```

In `cmdUnlock`, keep the same messages and exit codes: on error print `unlock: <err>` (for the not-a-repo case keep `unlock: not inside a git repo; name one: git-sync unlock <repo>`) and return 2. Run `go test ./cmd/...` to confirm `unlock` behaviour is unchanged.

3. New command:

```go
// cmdActivate is `git-sync activate [<repo>]` for humans, and
// `git-sync activate --drain` for the background drainer.
func cmdActivate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--drain" {
		return syncer.DrainActivate()
	}
	if len(args) > 1 {
		fmt.Fprintln(stderr, "usage: git-sync activate [<repo>]")
		return 2
	}
	rel := ""
	if len(args) == 1 {
		rel = args[0]
	} else {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(stderr, "activate:", err)
			return 1
		}
		if rel, err = currentRel(cfg); err != nil {
			fmt.Fprintln(stderr, "activate:", err)
			return 2
		}
	}
	return syncer.ActivateNow(rel, stdout)
}
```

`main.go`: in the human section of the `switch`, add

```go
	case "activate":
		return cmdActivate(args[1:], stdout, stderr)
```

and add a usage line after `unlock`:

```
  git-sync activate [<repo>]           run a repo's ./activate now (default: this repo)
```

- [ ] **Step 4: Run to verify pass**

Run: `make check`
Expected: vet clean, `gofmt -l` prints nothing, all tests PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w cmd internal
git add cmd internal/syncer
git commit -m "feat(activate): start the drainer after receive, announce and watch"
```

### Task 7: End-to-end: a commit on A activates on B, not on A

**Files:**
- Test: `internal/syncer/e2e_test.go`

- [ ] **Step 1: Write the test**

```go
func TestEndToEndCommitRunsActivateOnThePeerOnly(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newMachine(t, bin, "peerhost")
	peer.clone(t, sb, "group/proj")
	peer.saveConfig(t, []string{"group/proj"}, []config.Peer{{Host: "machineA", User: "tester"}})
	installLoopbackSSH(t, sb, peer)

	if err := os.WriteFile(filepath.Join(repo, "activate"),
		[]byte("#!/bin/sh\necho \"$GITSYNC_NEW_REV\" > \"$HOME/activated\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb.Git(repo, "add", "activate")
	sb.Git(repo, "commit", "-qm", "add activate")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	// The peer's receive starts a detached drainer; give it time.
	marker := filepath.Join(peer.Home, "activated")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("./activate never ran on the peer; events: %+v", peer.events(t))
		}
		time.Sleep(100 * time.Millisecond)
	}
	head := strings.TrimSpace(sb.Git(repo, "rev-parse", "HEAD"))
	testutil.AssertFileContains(t, marker, head)
	if _, err := os.Stat(filepath.Join(sb.Home, "activated")); err == nil {
		t.Error("./activate ran on the committing machine too")
	}
	// Wait for the drainer's final event so it is not still writing when
	// the temp dirs are cleaned up.
	deadline = time.Now().Add(10 * time.Second)
	for !hasEvent(peer.events(t), activity.OpActivate, activity.StatusOK, "activated") {
		if time.Now().After(deadline) {
			t.Fatalf("no activate ok event on the peer: %+v", peer.events(t))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run**

Run: `go test -race -run TestEndToEndCommitRunsActivateOnThePeerOnly ./internal/syncer/`
Expected: PASS. If the marker never appears, check that the loopback stub's routed environment gives the detached drainer the peer's `HOME`/`GITSYNC_HOME`. `spawnDetached` inherits the receive's environment, so it should.

- [ ] **Step 3: Commit**

```bash
git add internal/syncer/e2e_test.go
git commit -m "test(e2e): a synced commit runs ./activate on the peer only"
```

### Task 8: Docs, full check, rebuild

**Files:**
- Modify: `CLAUDE.md`, `README.md`

- [ ] **Step 1: Update `CLAUDE.md`**
  - Subcommand count: twelve. `activate` is human-facing (`activate [<repo>]`), and `activate --drain` is machine-invoked.
  - Under `internal/syncer`, add an `activate.go` bullet: queue under `~/.gitsync/activate/queue/` (one file per repo, `link(2)` create-if-absent, keeps the oldest rev); `receive` only enqueues after a fast-forward that moved HEAD; the command layer starts the drainer after receive, announce and watch (`WatchOptions.AfterAnnounce`); one machine-wide lock (`locks/.activate.lock`) keeps runs serial; each run holds the repo's receive lock (`Owner.From == "./activate"`); env `GITSYNC_OLD_REV`/`GITSYNC_NEW_REV` (empty tree on a manual run), `GITSYNC_INTERNAL` stripped; logs in `~/.gitsync/activate/<rel>.log`; `GITSYNC_ACTIVATE_TIMEOUT` (default 15m) is a test escape hatch.
  - Runtime layout: add `activate/`.
  - Key invariants: add "Only receiving machines run `./activate`, one at a time; a failed one is an event, never retried automatically."
- [ ] **Step 2: Update `README.md`.** Add a short "Making synced code live: `./activate`" section with the contract from the spec (executable, idempotent, no tty, sets its own PATH, env vars, exit status) and the two-line service example.
- [ ] **Step 3: Full check**

Run: `make check && go test -race ./...`
Expected: all PASS.

- [ ] **Step 4: Rebuild**

Run: `make build`
Expected: `built .../bin/git-sync-<os>-<arch>` for all four targets.

- [ ] **Step 5: Commit**

```bash
git add CLAUDE.md README.md
git commit -m "docs: document ./activate"
```

- [ ] **Step 6: Roll out the binary.** Run `git-sync service install`, which also upgrades each machine's binary. Don't run it without asking the user first: it touches every machine in the mesh.
