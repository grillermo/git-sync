# git-sync-status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A macOS menu bar app, `git-sync-status`, that shows whether git-sync on this Mac is syncing, has succeeded, has problems, or has pending deliveries. It reads everything from a new `git-sync status --json --follow` command.

**Architecture:** The Go side gains an `offline` event status, a per-operation run id on receive events, in-flight marker files (`internal/running`), and a pure aggregation package (`internal/status`) behind a hidden `status` subcommand that streams one JSON snapshot per line. The Swift side is a SwiftPM package in `git-sync-status/`. It has a pure `GitSyncStatusCore` library (decoding, row mapping, tested) and an AppKit/SwiftUI executable (status item, animated icon, transient popover table). The executable runs `~/.gitsync/bin/git-sync status --json --follow` and redraws on every line.

**Tech Stack:** Go 1.26 (stdlib only for the new code), Swift 6.2 toolchain with Swift 5 language mode, AppKit + SwiftUI, swift-testing, `rsvg-convert` for the icon.

**Spec:** `docs/superpowers/specs/2026-10-09-git-sync-status-design.md`

## Refinements to the spec (the plan wins; Task 14 writes them back into the spec)

1. **Run ids on receive events.** A single receive writes several events, for example `ok` "stashed", `warn` "diverged", `ok` "restored stashed changes". "Latest event per channel" would let the trailing `ok` hide the `warn`. So `activity.Event` gains `Run`, and `syncRepo` stamps every event of one receive with the same id. A channel's problems are the warn/error events of its **latest run**. Events with no `Run` (every other op, and old log lines) are each their own run.
2. **More offline cases.** The receiver's own fetch failure is `offline` when `gitcmd.IsOffline` says so (otherwise `error`). `announce`'s "could not reach the remote at startup" is also `offline`.
3. **`markPending` keeps the first time.** It no longer truncates an existing marker, so its mtime is when the delivery was first queued, which is what the "since" column shows.
4. **One icon image, straight into the `.app`.** The whole glyph rotates while syncing (arrows and git mark together, decided after the spec), so there is one layer, not two. No SwiftPM resources and no `Bundle.module`. `build` renders `icon.png` (+`@2x`) at 22pt into `Contents/Resources`, and the app loads it with `Bundle.main`. `Sources/GitSyncStatus/Resources/` does not exist. The SVG's canvas is `5 5 90 90`, wide enough that no corner clips at any angle, which is why the icon is drawn at 22pt rather than 18.
5. **Two Swift targets.** `GitSyncStatusCore` is a library with no AppKit and is unit-tested. `GitSyncStatus` is the executable. The package uses Swift 5 language mode, so strict-concurrency diagnostics don't block the build.
6. **Icon colour and badge.** The icon is drawn in `NSColor.labelColor` at display time. It is a template image when there is no problem, and a non-template image when the red dot must stay red. The dot never rotates, and a transparent ring is cleared around it so it reads as a badge on top of the arrows (see `git-sync-status/assets/icon-states-preview.png`).
7. **The window's table is a hand-laid grid** (`LazyVStack` of fixed-width columns), not SwiftUI `Table`. That allows a "Pending" header inside the list and a single tap target per row.
8. `build` runs `swift test` before installing, so a failing test never replaces the running app.

## File map

| File | Responsibility |
|---|---|
| `internal/activity/event.go` | `StatusOffline`, `Event.Run`, `NewRun()` |
| `internal/config/config.go` | `RunningDir()` |
| `internal/running/running.go` (new) | in-flight marker files: `Start`, `List` |
| `internal/syncer/push.go`, `receive.go`, `announce.go` | offline statuses, run id, running markers |
| `internal/syncer/pending.go` | `PendingEntry`, `ListAllPending`, non-truncating `markPending` |
| `internal/syncer/activate.go` | running marker around `runActivate` |
| `internal/status/build.go` (new) | snapshot types + pure `Build` |
| `internal/status/follow.go` (new) | `Collect`, `Fingerprint`, `Watched`, `Follow`, `WriteText` |
| `cmd/git-sync/main.go`, `stubs.go` | hidden `status` subcommand |
| `git-sync-status/Package.swift` (new) | SwiftPM manifest |
| `git-sync-status/Sources/GitSyncStatusCore/*.swift` (new) | `Snapshot`, `Row`, `rows(for:)`, `ago`, `IconState` |
| `git-sync-status/Sources/GitSyncStatus/*.swift` (new) | app: `main`, `AppDelegate`, `StatusFeed`, `IconRenderer`, `StatusItemController`, `StatusTable` |
| `git-sync-status/Tests/GitSyncStatusCoreTests/CoreTests.swift` (new) | swift-testing tests for Core |
| `git-sync-status/build`, `.gitignore` | build/install script |

Run Go commands from the repo root. Before every Go commit: `gofmt -w` on the touched packages, then `make check`.

---

### Task 1: `offline` status and run ids in the activity log

**Files:**
- Modify: `internal/activity/event.go`
- Test: `internal/activity/log_test.go`, `internal/report/aggregate_test.go`

- [ ] **Step 1: Write the failing tests**

In `internal/activity/log_test.go`, add a row to the `TestEventIsProblem` cases table:

```go
		{activity.StatusOffline, false},
```

and append:

```go
func TestNewRunIsUniquePerCall(t *testing.T) {
	a, b := activity.NewRun(), activity.NewRun()
	if a == "" || a == b {
		t.Errorf("NewRun() = %q then %q, want two distinct non-empty ids", a, b)
	}
}

func TestRunRoundTrips(t *testing.T) {
	testutil.NewSandbox(t)
	if err := activity.Append(activity.Event{Repo: "r", Op: activity.OpReceive, Status: activity.StatusOK, Run: "42.7"}); err != nil {
		t.Fatal(err)
	}
	got, _ := activity.Read()
	if len(got) != 1 || got[0].Run != "42.7" {
		t.Errorf("Run did not round-trip: %+v", got)
	}
}
```

In `internal/report/aggregate_test.go`, append:

```go
func TestSummarizeDoesNotCountOfflineAsAProblem(t *testing.T) {
	got := report.Summarize([]activity.Event{
		ev("a/one", activity.OpNotify, activity.StatusOffline, 1),
	})
	if got[0].Problems != 0 {
		t.Errorf("Problems = %d, want 0: an offline peer is queued, not failed", got[0].Problems)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail to compile**

Run: `go test ./internal/activity/ ./internal/report/`
Expected: FAIL, `undefined: activity.StatusOffline`, `undefined: activity.NewRun`, `unknown field Run`.

- [ ] **Step 3: Implement**

In `internal/activity/event.go`, extend the status block:

```go
const (
	StatusOK      Status = "ok"      // it worked
	StatusSkip    Status = "skip"    // deliberately did nothing; not a problem
	StatusWarn    Status = "warn"    // needs a human eventually (diverged, conflict)
	StatusError   Status = "error"   // it failed
	StatusOffline Status = "offline" // a machine was out of reach; queued and retried, not a problem
)
```

Add `Run` as the last field of `Event`:

```go
	Peer   string    `json:"peer,omitempty"`
	// Run groups the events one operation writes (a receive logs several),
	// so a reader can tell a problem in the latest run from an older one.
	// Empty means the event is a run of its own.
	Run string `json:"run,omitempty"`
```

Then append to the file, adding `"fmt"` and `"os"` to the imports:

```go
// NewRun returns a fresh id for Event.Run: unique per call, and per process
// across machines' worth of processes, because it includes the pid and the
// clock.
func NewRun() string {
	return fmt.Sprintf("%d.%d", os.Getpid(), time.Now().UnixNano())
}
```

`IsProblem` is unchanged: offline is neither warn nor error. `report`'s TUI already draws unknown statuses with `dimStyle` (`statusStyle`'s default case), and `plain.go` prints the status string as is. So `report` needs no code change.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/activity/ ./internal/report/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/activity ./internal/report && make check
git add internal/activity internal/report
git commit -m "feat(activity): offline status and per-run event ids

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Log out-of-reach deliveries as `offline`, and stamp receive runs

**Files:**
- Modify: `internal/syncer/push.go:84-96` and `:150-155`, `internal/syncer/receive.go:108-136`, `internal/syncer/announce.go:79-82`
- Test: `internal/syncer/push_test.go`, `internal/syncer/pending_test.go`, `internal/syncer/receive_test.go`

- [ ] **Step 1: Update the existing tests to the new status, and add new ones**

`internal/syncer/push_test.go`: rename `TestPushRecordsAnUnreachablePeerAsAnError` to `TestPushRecordsAnUnreachablePeerAsOffline` and change its assertion to:

```go
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOffline, "unreachable")
	testutil.AssertNoEvent(t, activity.OpNotify, activity.StatusError)
```

In the same file, in the test that asserts `"down.local"` (around line 237), change `activity.StatusError` to `activity.StatusOffline`. Then append:

```go
func TestPushRecordsAPeerThatCouldNotFetchAsOffline(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(syncer.ExitFetchFailed)
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOffline, "could not fetch")
}
```

`internal/syncer/pending_test.go`, in `TestAPushMadeOfflineIsPushedByTheNextRun`, change:

```go
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOffline, "will retry")
```

`internal/syncer/receive_test.go`: append (add `"os"` to the imports if it's missing):

```go
func TestReceiveRecordsAnUnreachableRemoteAsOffline(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	// Nothing listens on port 1: git fails with "Connection refused".
	sb.Git(repo, "remote", "set-url", "origin", "http://127.0.0.1:1/proj.git")

	if code := syncer.Receive("group/proj", "peer.example"); code != syncer.ExitFetchFailed {
		t.Errorf("Receive = %d, want ExitFetchFailed", code)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusOffline, "fetch")
	testutil.AssertNoEvent(t, activity.OpReceive, activity.StatusError)
}

func TestReceiveGroupsItsEventsUnderOneRun(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from-peer")
	sb.Dirty(repo) // stash, fast-forward, pop: several events in one receive

	if code := syncer.Receive("group/proj", "peer.example"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	events, err := activity.Read()
	if err != nil {
		t.Fatal(err)
	}
	runs := map[string]int{}
	for _, e := range events {
		if e.Op == activity.OpReceive {
			runs[e.Run]++
		}
	}
	if len(runs) != 1 || runs[""] != 0 {
		t.Fatalf("want every receive event under one non-empty run, got %v", runs)
	}
	for _, n := range runs {
		if n < 3 {
			t.Errorf("want at least 3 events in the run (stash, fast-forward, restore), got %d", n)
		}
	}
}
```

The existing `TestReceiveReportsAFailedFetch` (an origin path that doesn't exist, which is not a network failure) keeps asserting `StatusError`. That test now pins the "not offline" side of the split.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/syncer/ -run 'Offline|OneRun|Unreachable|MadeOffline|down|Fetch'`
Expected: FAIL. The events are still `error`, and `Run` is empty.

- [ ] **Step 3: Implement**

`internal/syncer/push.go`, in `pushRepo`, replace the failed-push block with:

```go
	if _, err := gitcmd.Push(dir, remote, branch); err != nil {
		msg := "push to " + remote + " failed: " + gitcmd.Summary(err)
		st := activity.StatusError
		if gitcmd.IsOffline(err) {
			markPending(pendingPush, rel)
			msg += " (will retry)"
			st = activity.StatusOffline
		} else {
			clearPending(pendingPush, rel)
		}
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpPush, Status: st,
			Branch: branch, Msg: msg,
		})
		return nil
	}
```

In `notifyPeer`'s switch:

```go
	case code == 255:
		ev.Status, ev.Msg = activity.StatusOffline, "peer "+p.Host+" unreachable, will retry"
		delivered = false
	case code == ExitFetchFailed:
		ev.Status, ev.Msg = activity.StatusOffline, "peer "+p.Host+" could not fetch, will retry"
		delivered = false
```

`internal/syncer/receive.go`, in `syncRepo`, replace the `log` closure and the fetch block:

```go
	// One run id for every event this receive writes, so a reader can tell
	// this receive's warning from an older one (see activity.Event.Run).
	run := activity.NewRun()
	log := func(s activity.Status, branch, msg string) {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpReceive, Status: s, Branch: branch, Msg: msg, Run: run,
		})
	}
```

```go
	if err := gitcmd.Fetch(dir, remote); err != nil {
		// Not 0: the notifier must know this machine missed the commit, so it
		// can queue the notification and try again later.
		st := activity.StatusError
		if gitcmd.IsOffline(err) {
			st = activity.StatusOffline
		}
		log(st, branch, "fetch from "+remote+" failed: "+gitcmd.Summary(err))
		return ExitFetchFailed
	}
```

`internal/syncer/announce.go`, in the deadline loop, change `Status: activity.StatusError` to `Status: activity.StatusOffline`.

- [ ] **Step 4: Run the whole syncer suite**

Run: `go test ./internal/syncer/...`
Expected: PASS. If an e2e test fails on `assertNoErrorEvents`, check it before changing anything. That helper only looks at `error`, so `offline` events can't trip it.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/syncer && make check
git add internal/syncer
git commit -m "feat(syncer): log out-of-reach deliveries as offline

A peer or remote that is merely unreachable is queued and retried; it is
not a failure. Receive events now share a run id.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: List the pending queue, and date each entry by when it was first queued

**Files:**
- Modify: `internal/syncer/pending.go`
- Test: `internal/syncer/pending_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/syncer/pending_test.go` (add `"net/url"` and `"time"` to its imports if they're missing):

```go
func TestListAllPendingReturnsPushesAndNotifies(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, b := twoRepoMesh(t, sb, "peer.local")
	sb.StubSSH(255)
	testutil.Commit(t, sb, b, "notify me")
	syncer.Push("group/b") // pushed, but the peer is unreachable

	sb.Git(a, "remote", "set-url", "origin", "http://127.0.0.1:1/a.git")
	testutil.Commit(t, sb, a, "made offline")
	syncer.Push("group/a") // the push itself is unreachable

	got := map[string]syncer.PendingEntry{}
	for _, e := range syncer.ListAllPending() {
		got[e.Kind+" "+e.Host+" "+e.Rel] = e
	}
	if _, ok := got["push  group/a"]; !ok {
		t.Errorf("missing the queued push: %+v", got)
	}
	if _, ok := got["notify peer.local group/b"]; !ok {
		t.Errorf("missing the queued notify: %+v", got)
	}
	for k, e := range got {
		if e.Since.IsZero() {
			t.Errorf("%s has no Since", k)
		}
	}
}

func TestRequeueingKeepsTheFirstQueuedTime(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, _ := twoRepoMesh(t, sb, "peer.local")
	sb.StubSSH(0)
	sb.Git(a, "remote", "set-url", "origin", "http://127.0.0.1:1/a.git")
	testutil.Commit(t, sb, a, "first")
	syncer.Push("group/a")

	marker := filepath.Join(sb.GitsyncHome, "pending", "push", url.PathEscape("group/a"))
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, sb, a, "second")
	syncer.Push("group/a") // still offline: queued again

	for _, e := range syncer.ListAllPending() {
		if e.Rel == "group/a" && !e.Since.Equal(old) {
			t.Errorf("Since = %v, want the first queued time %v", e.Since, old)
		}
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/syncer/ -run 'ListAllPending|FirstQueued'`
Expected: FAIL to compile: `undefined: syncer.ListAllPending`.

- [ ] **Step 3: Implement**

In `internal/syncer/pending.go`, change `markPending`'s create so it never truncates (truncating an existing file bumps its mtime):

```go
	// No O_TRUNC: re-queueing an existing entry must keep its mtime, which
	// is when the delivery was first missed (git-sync status shows it).
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
```

Append (add `"sort"` and `"time"` to the imports):

```go
// PendingEntry is one missed delivery, for git-sync status. Kind is "push"
// or "notify"; Host is set for notify only. Since is when it was first
// queued.
type PendingEntry struct {
	Kind  string
	Rel   string
	Host  string
	Since time.Time
}

// ListAllPending returns every queued delivery, oldest first.
func ListAllPending() []PendingEntry {
	var out []PendingEntry
	add := func(kind, host, dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
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
			out = append(out, PendingEntry{Kind: kind, Rel: rel, Host: host, Since: info.ModTime()})
		}
	}
	add(pendingPush, "", pendingPath(pendingPush))
	hosts, _ := os.ReadDir(pendingPath(pendingNotify))
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		host, err := url.PathUnescape(h.Name())
		if err != nil {
			continue
		}
		add(pendingNotify, host, filepath.Join(pendingPath(pendingNotify), h.Name()))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/syncer/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/syncer && make check
git add internal/syncer
git commit -m "feat(syncer): list the pending queue with first-queued times

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `internal/running` — in-flight markers

**Files:**
- Modify: `internal/config/config.go` (path helpers block, ~line 93)
- Create: `internal/running/running.go`
- Test: `internal/running/running_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/running/running_test.go`:

```go
package running_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestStartIsListedUntilStopped(t *testing.T) {
	testutil.NewSandbox(t)
	stop := running.Start("group/proj", activity.OpNotify, "peer.local")

	got := running.List()
	if len(got) != 1 {
		t.Fatalf("List = %+v, want one marker", got)
	}
	m := got[0]
	if m.Repo != "group/proj" || m.Op != activity.OpNotify || m.Peer != "peer.local" || m.PID != os.Getpid() || m.Started.IsZero() {
		t.Errorf("marker = %+v", m)
	}

	stop()
	stop() // idempotent
	if got := running.List(); len(got) != 0 {
		t.Errorf("after stop, List = %+v, want none", got)
	}
}

func TestTwoOperationsOfOneProcessAreBothListed(t *testing.T) {
	testutil.NewSandbox(t)
	defer running.Start("group/proj", activity.OpNotify, "a.local")()
	defer running.Start("group/proj", activity.OpNotify, "b.local")()
	if got := running.List(); len(got) != 2 {
		t.Errorf("List = %+v, want two markers", got)
	}
}

func TestListDropsAndRemovesMarkersOfDeadProcesses(t *testing.T) {
	testutil.NewSandbox(t)
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.ProcessState.Pid()
	testutil.MkdirAll(t, config.RunningDir())
	path := filepath.Join(config.RunningDir(), fmt.Sprintf("%d-x.json", dead))
	body := fmt.Sprintf(`{"repo":"r","op":"push","started":"2026-10-09T12:00:00Z","pid":%d}`, dead)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := running.List(); len(got) != 0 {
		t.Errorf("List = %+v, want the dead process's marker dropped", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a dead process's marker should be removed")
	}
}

func TestListSkipsUnreadableMarkers(t *testing.T) {
	testutil.NewSandbox(t)
	testutil.MkdirAll(t, config.RunningDir())
	// Our own pid, so it is alive, but the body is half-written.
	path := filepath.Join(config.RunningDir(), fmt.Sprintf("%d-y.json", os.Getpid()))
	if err := os.WriteFile(path, []byte(`{"repo":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := running.List(); len(got) != 0 {
		t.Errorf("List = %+v, want the torn marker skipped", got)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/running/`
Expected: FAIL to compile (no package, no `config.RunningDir`).

- [ ] **Step 3: Implement**

`internal/config/config.go`: add the helper next to `PendingDir`:

```go
func RunningDir() string   { return filepath.Join(Home(), "running") }
```

Create `internal/running/running.go`:

```go
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
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/running/ ./internal/config/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/running ./internal/config && make check
git add internal/running internal/config
git commit -m "feat(running): in-flight operation markers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Mark push, notify, receive and ./activate as running

**Files:**
- Modify: `internal/syncer/push.go` (`pushRepo`, `notifyPeer`), `internal/syncer/receive.go` (`receive`), `internal/syncer/activate.go` (`runActivate`)
- Test: `internal/syncer/running_test.go` (new)

- [ ] **Step 1: Write the failing test**

Create `internal/syncer/running_test.go`:

```go
package syncer_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestActivateIsMarkedRunningWhileItRuns(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	seen := filepath.Join(sb.Home, "seen")
	script := "#!/bin/sh\ncat \"$GITSYNC_HOME\"/running/* > '" + seen + "'\n"
	if err := os.WriteFile(filepath.Join(repo, "activate"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if code := syncer.ActivateNow("group/proj", io.Discard); code != 0 {
		t.Fatalf("ActivateNow = %d, want 0", code)
	}
	testutil.AssertFileContains(t, seen, `"op":"activate"`)
	if m := running.List(); len(m) != 0 {
		t.Errorf("marker left behind: %+v", m)
	}
}

func TestReceiveIsMarkedRunningWhileItRuns(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	// A fake git first on PATH records the running dir at fetch time, then
	// hands over to the real git.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(sb.Home, "seen")
	testutil.WriteScript(t, sb, "git", "#!/bin/sh\n"+
		"[ \"$1\" = fetch ] && cat \"$GITSYNC_HOME\"/running/* > '"+seen+"'\n"+
		"exec '"+realGit+"' \"$@\"\n")
	t.Setenv("PATH", filepath.Join(sb.Home, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	syncer.Receive("group/proj", "peer.example")
	testutil.AssertFileContains(t, seen, `"op":"receive"`)
	testutil.AssertFileContains(t, seen, `"peer":"peer.example"`)
}

func TestPushAndReceiveLeaveNoRunningMarker(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	syncer.Receive("group/proj", "peer.example")
	entries, _ := os.ReadDir(config.RunningDir())
	if len(entries) != 0 {
		t.Errorf("running/ not empty after push and receive: %v", entries)
	}
}
```

Add `"os/exec"` to that file's imports. The fake git works because `gitcmd` starts git as `exec.Command("git", …)`, which resolves through `PATH`.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/syncer/ -run 'MarkedRunning|NoRunningMarker'`
Expected: the two `MarkedRunning` tests FAIL, because `seen` doesn't contain the op. `NoRunningMarker` passes already, and it's there to guard the cleanup.

- [ ] **Step 3: Implement**

Add `"github.com/grillermo/git-sync/internal/running"` to the imports of `push.go`, `receive.go` and `activate.go`.

`push.go`, first line of `pushRepo`'s body:

```go
	defer running.Start(rel, activity.OpPush, "")()
```

`push.go`, first line of `notifyPeer`'s body:

```go
	defer running.Start(rel, activity.OpNotify, p.Host)()
```

`receive.go`, in `receive`, right after `stop := heartbeat(l); defer stop()`:

```go
	defer running.Start(rel, activity.OpReceive, from)()
```

`activate.go`, first line of `runActivate`'s body:

```go
	defer running.Start(rel, activity.OpActivate, "")()
```

- [ ] **Step 4: Run the syncer suite**

Run: `go test -race ./internal/syncer/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/syncer && make check
git add internal/syncer
git commit -m "feat(syncer): mark push, notify, receive and activate as running

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `internal/status` — the pure snapshot

**Files:**
- Create: `internal/status/build.go`
- Test: `internal/status/build_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/status/build_test.go`:

```go
package status_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/status"
	"github.com/grillermo/git-sync/internal/syncer"
)

var base = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

func cfg(repos ...string) config.Config { return config.Config{BaseDir: "/code", Repos: repos} }

func ev(min int, repo string, op activity.Op, st activity.Status, msg string) activity.Event {
	return activity.Event{Time: at(min), Repo: repo, Op: op, Status: st, Msg: msg}
}

func notify(min int, repo, peer string, st activity.Status, msg string) activity.Event {
	e := ev(min, repo, activity.OpNotify, st, msg)
	e.Peer = peer
	return e
}

func build(c config.Config, events ...activity.Event) status.Snapshot {
	return status.Build(status.Input{Config: c, Events: events, Now: at(60)})
}

func repo(t *testing.T, s status.Snapshot, rel string) status.Repo {
	t.Helper()
	for _, r := range s.Repos {
		if r.Repo == rel {
			return r
		}
	}
	t.Fatalf("no repo %q in %+v", rel, s.Repos)
	return status.Repo{}
}

func TestARepoWithNoActivityIsNever(t *testing.T) {
	r := repo(t, build(cfg("a")), "a")
	if r.State != status.StateNever || r.LastSync != nil || r.Path != "/code/a" {
		t.Errorf("got %+v", r)
	}
}

func TestALaterSuccessOnTheSameChannelClearsAProblem(t *testing.T) {
	s := build(cfg("a"),
		ev(1, "a", activity.OpPush, activity.StatusError, "rejected"),
		ev(2, "a", activity.OpPush, activity.StatusOK, "pushed"),
	)
	r := repo(t, s, "a")
	if r.State != status.StateOK || len(r.Problems) != 0 || s.Problems != 0 {
		t.Errorf("got %+v", r)
	}
	if r.LastSync == nil || !r.LastSync.Equal(at(2)) {
		t.Errorf("LastSync = %v, want %v", r.LastSync, at(2))
	}
}

func TestASuccessOnAnotherChannelDoesNotClearAProblem(t *testing.T) {
	s := build(cfg("a"),
		notify(1, "a", "mini", activity.StatusError, "peer mini receive failed (exit 1)"),
		ev(2, "a", activity.OpPush, activity.StatusOK, "pushed"),
		notify(2, "a", "laptop", activity.StatusOK, "peer laptop synced"),
	)
	r := repo(t, s, "a")
	if r.State != status.StateError || len(r.Problems) != 1 || r.Problems[0].Peer != "mini" {
		t.Errorf("got %+v", r)
	}
	if s.Problems != 1 {
		t.Errorf("Snapshot.Problems = %d, want 1", s.Problems)
	}
}

func TestOfflineIsNeverAProblem(t *testing.T) {
	s := build(cfg("a", "b"),
		notify(1, "a", "pc", activity.StatusOffline, "peer pc unreachable, will retry"),
		// A line written before the offline status existed.
		notify(1, "b", "pc", activity.StatusError, "peer pc unreachable, will retry"),
	)
	for _, rel := range []string{"a", "b"} {
		if r := repo(t, s, rel); len(r.Problems) != 0 {
			t.Errorf("%s: offline counted as a problem: %+v", rel, r)
		}
	}
}

func TestOfflineDoesNotClearAProblem(t *testing.T) {
	r := repo(t, build(cfg("a"),
		notify(1, "a", "pc", activity.StatusError, "receive failed"),
		notify(2, "a", "pc", activity.StatusOffline, "unreachable, will retry"),
	), "a")
	if r.State != status.StateError {
		t.Errorf("got %+v", r)
	}
}

func TestSkipsAreIgnored(t *testing.T) {
	r := repo(t, build(cfg("a"),
		ev(1, "a", activity.OpPush, activity.StatusError, "rejected"),
		ev(2, "a", activity.OpPush, activity.StatusSkip, "detached HEAD"),
	), "a")
	if r.State != status.StateError {
		t.Errorf("got %+v", r)
	}
}

func receiveRun(min int, run string, st activity.Status, msg string) activity.Event {
	e := ev(min, "a", activity.OpReceive, st, msg)
	e.Run = run
	return e
}

func TestAWarningInsideTheLatestReceiveRunCounts(t *testing.T) {
	r := repo(t, build(cfg("a"),
		receiveRun(1, "r1", activity.StatusOK, "stashed dirty working tree"),
		receiveRun(1, "r1", activity.StatusWarn, "diverged, manual merge needed"),
		receiveRun(1, "r1", activity.StatusOK, "restored stashed changes"),
	), "a")
	if r.State != status.StateWarn || len(r.Problems) != 1 {
		t.Errorf("got %+v", r)
	}
}

func TestANewReceiveRunClearsTheLastRunsWarning(t *testing.T) {
	r := repo(t, build(cfg("a"),
		receiveRun(1, "r1", activity.StatusWarn, "diverged"),
		receiveRun(5, "r2", activity.StatusOK, "fast-forwarded main from origin"),
	), "a")
	if r.State != status.StateOK || len(r.Problems) != 0 {
		t.Errorf("got %+v", r)
	}
}

func TestRunningMakesARepoSyncingAndKeepsItsProblems(t *testing.T) {
	s := status.Build(status.Input{
		Config:  cfg("a"),
		Events:  []activity.Event{ev(1, "a", activity.OpPush, activity.StatusError, "rejected")},
		Running: []running.Marker{{Repo: "a", Op: activity.OpPush, Started: at(59)}},
		Now:     at(60),
	})
	r := repo(t, s, "a")
	if !s.Syncing || r.State != status.StateSyncing || len(r.Running) != 1 || len(r.Problems) != 1 {
		t.Errorf("got syncing=%v %+v", s.Syncing, r)
	}
}

func TestReposAreOrderedSyncingErrorWarnThenRecent(t *testing.T) {
	s := status.Build(status.Input{
		Config: cfg("old", "recent", "warned", "failed", "busy"),
		Events: []activity.Event{
			ev(1, "old", activity.OpPush, activity.StatusOK, ""),
			ev(9, "recent", activity.OpPush, activity.StatusOK, ""),
			ev(2, "warned", activity.OpPush, activity.StatusWarn, ""),
			ev(3, "failed", activity.OpPush, activity.StatusError, ""),
		},
		Running: []running.Marker{{Repo: "busy", Op: activity.OpReceive, Started: at(59)}},
		Now:     at(60),
	})
	var got []string
	for _, r := range s.Repos {
		got = append(got, r.Repo)
	}
	want := "busy failed warned recent old"
	if strings.Join(got, " ") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

func TestUnselectedReposAreLeftOut(t *testing.T) {
	s := status.Build(status.Input{
		Config:  cfg("a"),
		Events:  []activity.Event{ev(1, "gone", activity.OpPush, activity.StatusError, "x")},
		Running: []running.Marker{{Repo: "gone", Op: activity.OpPush, Started: at(1)}},
		Pending: []syncer.PendingEntry{{Kind: "push", Rel: "gone", Since: at(1)}},
		Now:     at(60),
	})
	if len(s.Repos) != 1 || len(s.Pending) != 0 || s.Syncing || s.Problems != 0 {
		t.Errorf("got %+v", s)
	}
}

func TestPendingIsListedOldestFirstWithPaths(t *testing.T) {
	s := status.Build(status.Input{
		Config: cfg("a", "b"),
		Pending: []syncer.PendingEntry{
			{Kind: "notify", Rel: "b", Host: "pc", Since: at(5)},
			{Kind: "push", Rel: "a", Since: at(2)},
		},
		Now: at(60),
	})
	if len(s.Pending) != 2 || s.Pending[0].Repo != "a" || s.Pending[1].Peer != "pc" || s.Pending[1].Path != "/code/b" {
		t.Errorf("got %+v", s.Pending)
	}
}

func TestSnapshotJSONHasNoNullLists(t *testing.T) {
	b, err := json.Marshal(build(cfg("a")))
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{`"repos":[`, `"pending":[]`, `"running":[]`, `"problems":[]`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON lacks %s: %s", want, js)
		}
	}
	if strings.Contains(js, "null") {
		t.Errorf("JSON has a null: %s", js)
	}
}

func TestTimesAreWholeSeconds(t *testing.T) {
	e := ev(1, "a", activity.OpPush, activity.StatusOK, "")
	e.Time = e.Time.Add(123 * time.Millisecond)
	r := repo(t, build(cfg("a"), e), "a")
	if r.LastSync.Nanosecond() != 0 {
		t.Errorf("LastSync %v keeps fractional seconds; the Swift decoder rejects them", r.LastSync)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/status/`
Expected: FAIL to compile (no package).

- [ ] **Step 3: Implement**

Create `internal/status/build.go`:

```go
// Package status condenses this machine's activity log, pending queue and
// in-flight operations into one snapshot of every selected repo, for the
// hidden `git-sync status` command and the git-sync-status menu bar app
// that reads it. Build is pure; follow.go holds the I/O.
package status

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/syncer"
)

// State is a repo's one-word summary, highest-ranked first.
type State string

const (
	StateSyncing State = "syncing"
	StateError   State = "error"
	StateWarn    State = "warn"
	StateOK      State = "ok"
	StateNever   State = "never"
)

// Snapshot is one line of `git-sync status --json`. Every list is non-nil,
// so it marshals as [] rather than null, and every time is whole seconds,
// so Swift's .iso8601 date decoding accepts it.
type Snapshot struct {
	At       time.Time `json:"at"`
	Syncing  bool      `json:"syncing"`
	Problems int       `json:"problems"` // repos with at least one problem
	Repos    []Repo    `json:"repos"`
	Pending  []Pending `json:"pending"`
	Error    string    `json:"error,omitempty"`
}

type Repo struct {
	Repo     string     `json:"repo"`
	Path     string     `json:"path"`
	State    State      `json:"state"`
	LastSync *time.Time `json:"last_sync,omitempty"`
	Running  []Running  `json:"running"`
	Problems []Problem  `json:"problems"` // newest first
}

type Running struct {
	Op      activity.Op `json:"op"`
	Peer    string      `json:"peer,omitempty"`
	Started time.Time   `json:"started"`
}

type Problem struct {
	Time   time.Time       `json:"ts"`
	Op     activity.Op     `json:"op"`
	Peer   string          `json:"peer,omitempty"`
	Status activity.Status `json:"status"`
	Msg    string          `json:"msg"`
}

type Pending struct {
	Kind  string    `json:"kind"` // "push" or "notify"
	Repo  string    `json:"repo"`
	Path  string    `json:"path"`
	Peer  string    `json:"peer,omitempty"`
	Since time.Time `json:"since"`
}

// Input is everything Build reads. Events are oldest first, as
// activity.Read returns them.
type Input struct {
	Config  config.Config
	Events  []activity.Event
	Running []running.Marker
	Pending []syncer.PendingEntry
	Now     time.Time
}

// channel is one independent line of work for a repo: its pushes, its
// notifies to one peer, its receives, its ./activate runs. A problem on one
// channel is cleared only by a later success on that same channel - a fine
// push says nothing about a peer whose receive failed.
type channel struct {
	repo string
	op   activity.Op
	peer string
}

// Build turns the inputs into a snapshot. For each channel only its latest
// run counts: the channel has a problem if that run logged a warn or an
// error. Skips never count, and neither does offline: an out-of-reach
// machine is reported through the pending queue instead.
func Build(in Input) Snapshot {
	snap := Snapshot{At: sec(in.Now), Repos: []Repo{}, Pending: []Pending{}}

	byChan := map[channel][]int{}
	lastSync := map[string]time.Time{}
	lastSeen := map[string]time.Time{}
	for i, e := range in.Events {
		if !in.Config.IsSelected(e.Repo) {
			continue
		}
		if e.Time.After(lastSeen[e.Repo]) {
			lastSeen[e.Repo] = e.Time
		}
		switch effective(e) {
		case activity.StatusSkip, activity.StatusOffline:
			continue
		case activity.StatusOK:
			if e.Time.After(lastSync[e.Repo]) {
				lastSync[e.Repo] = e.Time
			}
		}
		k := channel{e.Repo, e.Op, e.Peer}
		byChan[k] = append(byChan[k], i)
	}

	problems := map[string][]Problem{}
	for k, idx := range byChan {
		last := idx[len(idx)-1] // the log is append-only: file order is time order
		run := runOf(in.Events[last], last)
		for _, i := range idx {
			e := in.Events[i]
			if runOf(e, i) != run || !e.IsProblem() {
				continue
			}
			problems[k.repo] = append(problems[k.repo], Problem{
				Time: sec(e.Time), Op: e.Op, Peer: e.Peer, Status: e.Status, Msg: e.Msg,
			})
		}
	}

	for _, rel := range in.Config.Repos {
		r := Repo{Repo: rel, Path: in.Config.RepoPath(rel), Running: []Running{}, Problems: []Problem{}}
		r.Problems = append(r.Problems, problems[rel]...)
		sort.SliceStable(r.Problems, func(i, j int) bool { return r.Problems[i].Time.After(r.Problems[j].Time) })
		if t, ok := lastSync[rel]; ok {
			t = sec(t)
			r.LastSync = &t
		}
		for _, m := range in.Running {
			if m.Repo == rel {
				r.Running = append(r.Running, Running{Op: m.Op, Peer: m.Peer, Started: sec(m.Started)})
			}
		}
		r.State = stateOf(r)
		if len(r.Running) > 0 {
			snap.Syncing = true
		}
		if len(r.Problems) > 0 {
			snap.Problems++
		}
		snap.Repos = append(snap.Repos, r)
	}
	sort.SliceStable(snap.Repos, func(i, j int) bool {
		a, b := snap.Repos[i], snap.Repos[j]
		if rank(a.State) != rank(b.State) {
			return rank(a.State) < rank(b.State)
		}
		if ta, tb := lastSeen[a.Repo], lastSeen[b.Repo]; !ta.Equal(tb) {
			return ta.After(tb)
		}
		return a.Repo < b.Repo
	})

	for _, p := range in.Pending {
		if !in.Config.IsSelected(p.Rel) {
			continue
		}
		snap.Pending = append(snap.Pending, Pending{
			Kind: p.Kind, Repo: p.Rel, Path: in.Config.RepoPath(p.Rel), Peer: p.Host, Since: sec(p.Since),
		})
	}
	sort.SliceStable(snap.Pending, func(i, j int) bool {
		if !snap.Pending[i].Since.Equal(snap.Pending[j].Since) {
			return snap.Pending[i].Since.Before(snap.Pending[j].Since)
		}
		return snap.Pending[i].Repo < snap.Pending[j].Repo
	})
	return snap
}

// effective reads a log line written before StatusOffline existed: those
// logged an out-of-reach delivery as an error whose message promised a
// retry. Delete once such logs no longer matter.
func effective(e activity.Event) activity.Status {
	if e.Status == activity.StatusError && strings.Contains(e.Msg, "will retry") {
		return activity.StatusOffline
	}
	return e.Status
}

// runOf is the event's run id, or a unique one for an event that is a run
// of its own.
func runOf(e activity.Event, i int) string {
	if e.Run != "" {
		return e.Run
	}
	return "#" + strconv.Itoa(i)
}

func stateOf(r Repo) State {
	switch {
	case len(r.Running) > 0:
		return StateSyncing
	case hasStatus(r.Problems, activity.StatusError):
		return StateError
	case len(r.Problems) > 0:
		return StateWarn
	case r.LastSync != nil:
		return StateOK
	}
	return StateNever
}

func hasStatus(ps []Problem, s activity.Status) bool {
	for _, p := range ps {
		if p.Status == s {
			return true
		}
	}
	return false
}

func rank(s State) int {
	switch s {
	case StateSyncing:
		return 0
	case StateError:
		return 1
	case StateWarn:
		return 2
	}
	return 3
}

func sec(t time.Time) time.Time { return t.Truncate(time.Second) }
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/status/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/status && make check
git add internal/status
git commit -m "feat(status): build a per-repo sync snapshot

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Collect, change detection, follow loop and text output

**Files:**
- Create: `internal/status/follow.go`
- Test: `internal/status/follow_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/status/follow_test.go`:

```go
package status_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/status"
	"github.com/grillermo/git-sync/internal/testutil"
)

type lines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), "\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFollowPrintsAtStartAndOnEveryChange(t *testing.T) {
	var (
		mu sync.Mutex
		fp = "a"
	)
	out := &lines{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- status.Follow(ctx, out, status.FollowOptions{
			Collect:     func() status.Snapshot { return status.Snapshot{Repos: []status.Repo{}, Pending: []status.Pending{}} },
			Fingerprint: func() string { mu.Lock(); defer mu.Unlock(); return fp },
			Tick:        5 * time.Millisecond,
			Refresh:     time.Hour,
		})
	}()

	waitFor(t, "the first snapshot", func() bool { return out.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := out.count(); n != 1 {
		t.Fatalf("printed %d lines with nothing changed, want 1", n)
	}
	mu.Lock()
	fp = "b"
	mu.Unlock()
	waitFor(t, "a snapshot after the change", func() bool { return out.count() == 2 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Follow = %v, want nil on cancel", err)
	}
}

func TestFollowRefreshesWithoutAChange(t *testing.T) {
	out := &lines{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go status.Follow(ctx, out, status.FollowOptions{
		Collect:     func() status.Snapshot { return status.Snapshot{} },
		Fingerprint: func() string { return "same" },
		Tick:        5 * time.Millisecond,
		Refresh:     20 * time.Millisecond,
	})
	waitFor(t, "a refresh", func() bool { return out.count() >= 2 })
}

func TestFingerprintSeesAddedAndGrownFiles(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "log")
	before := status.Fingerprint(dir, filepath.Join(dir, "missing"))
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	added := status.Fingerprint(dir, filepath.Join(dir, "missing"))
	if added == before {
		t.Error("adding a file did not change the fingerprint")
	}
	if err := os.WriteFile(f, []byte("xy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status.Fingerprint(dir, filepath.Join(dir, "missing")) == added {
		t.Error("growing a file did not change the fingerprint")
	}
}

func TestCollectWithoutAConfigSaysNotInstalled(t *testing.T) {
	testutil.NewSandbox(t)
	s := status.Collect(time.Now())
	if !strings.Contains(s.Error, "not installed") || s.Repos == nil || s.Pending == nil {
		t.Errorf("got %+v", s)
	}
}

func TestWriteTextShowsReposAndPending(t *testing.T) {
	var b strings.Builder
	status.WriteText(&b, build(cfg("group/a")))
	if !strings.Contains(b.String(), "group/a") || !strings.Contains(b.String(), "never") {
		t.Errorf("text output:\n%s", b.String())
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/status/`
Expected: FAIL to compile: `undefined: status.Follow` and the others.

- [ ] **Step 3: Implement**

Create `internal/status/follow.go`:

```go
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
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/status/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/status && make check
git add internal/status
git commit -m "feat(status): collect, follow and text output

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: The hidden `git-sync status` subcommand

**Files:**
- Modify: `cmd/git-sync/main.go` (machine-invoked cases), `cmd/git-sync/stubs.go` (new `cmdStatus` after `cmdReport`)
- Test: `cmd/git-sync/main_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `cmd/git-sync/main_test.go` (add `"encoding/json"` and the `activity` and `status` imports):

```go
func TestStatusJSONReportsEachSelectedRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	_ = activity.Append(activity.Event{Repo: "group/proj", Op: activity.OpPush, Status: activity.StatusError, Msg: "rejected"})

	var out, errBuf bytes.Buffer
	if code := run([]string{"status", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("status --json = %d: %s", code, errBuf.String())
	}
	var s status.Snapshot
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("not one JSON snapshot: %v\n%s", err, out.String())
	}
	if len(s.Repos) != 1 || s.Repos[0].State != status.StateError || s.Problems != 1 {
		t.Errorf("got %+v", s)
	}
}

func TestStatusFollowNeedsJSON(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"status", "--follow"}, &out, &out); code != 2 {
		t.Errorf("status --follow without --json = %d, want 2", code)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./cmd/git-sync/ -run Status`
Expected: FAIL, `unknown subcommand "status"` (exit 2, so the JSON decode fails).

- [ ] **Step 3: Implement**

`cmd/git-sync/main.go`, in the machine-invoked block after `case "watch":`:

```go
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
```

`cmd/git-sync/stubs.go`: add `"encoding/json"` and `"github.com/grillermo/git-sync/internal/status"` to the imports, and after `cmdReport`:

```go
// cmdStatus is the hidden `git-sync status`: this machine's per-repo sync
// state. --json prints one snapshot as a JSON line; --follow keeps printing
// one per change until stdin closes or it is signalled - which is how the
// git-sync-status menu bar app reads it, and why it never outlives the app.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print a JSON snapshot")
	follow := fs.Bool("follow", false, "keep printing a snapshot on every change (needs --json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *follow && !*asJSON {
		fmt.Fprintln(stderr, "status: --follow needs --json")
		return 2
	}

	if !*follow {
		snap := status.Collect(time.Now())
		if !*asJSON {
			status.WriteText(stdout, snap)
			return 0
		}
		b, err := json.Marshal(snap)
		if err != nil {
			fmt.Fprintln(stderr, "status:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	err := status.Follow(ctx, stdout, status.FollowOptions{
		Collect:     func() status.Snapshot { return status.Collect(time.Now()) },
		Fingerprint: func() string { return status.Fingerprint(status.Watched()...) },
		Tick:        500 * time.Millisecond,
		Refresh:     time.Minute,
	})
	if err != nil {
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run the tests, the full suite, and try it by hand**

Run: `go test ./cmd/git-sync/ && make check`
Expected: PASS

Run: `go run ./cmd/git-sync status`
Expected: a table of this machine's real repos.

- [ ] **Step 5: Commit, rebuild, install locally**

```bash
gofmt -w ./cmd/git-sync && make check
git add cmd/git-sync
git commit -m "feat: hidden git-sync status command for the menu bar app

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
make build
./activate   # no GITSYNC_OLD_REV: builds and installs into ~/.gitsync/bin, which the app runs
~/.gitsync/bin/git-sync status --json | head -c 300
```

Expected: JSON that starts with `{"at":`.

---

### Task 9: Swift package skeleton and Core snapshot decoding

**Files:**
- Create: `git-sync-status/Package.swift`
- Create: `git-sync-status/Sources/GitSyncStatusCore/Snapshot.swift`
- Create: `git-sync-status/Sources/GitSyncStatus/main.swift` (placeholder that just compiles, replaced in Task 11)
- Test: `git-sync-status/Tests/GitSyncStatusCoreTests/CoreTests.swift`

- [ ] **Step 1: Write the manifest and the failing test**

`git-sync-status/Package.swift`:

```swift
// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "git-sync-status",
    platforms: [.macOS(.v15)],
    products: [
        .executable(name: "git-sync-status", targets: ["GitSyncStatus"]),
    ],
    targets: [
        .target(name: "GitSyncStatusCore", path: "Sources/GitSyncStatusCore"),
        .executableTarget(
            name: "GitSyncStatus",
            dependencies: ["GitSyncStatusCore"],
            path: "Sources/GitSyncStatus"
        ),
        .testTarget(
            name: "GitSyncStatusCoreTests",
            dependencies: ["GitSyncStatusCore"],
            path: "Tests/GitSyncStatusCoreTests"
        ),
    ],
    swiftLanguageModes: [.v5]
)
```

`git-sync-status/Sources/GitSyncStatus/main.swift` (temporary):

```swift
import GitSyncStatusCore
```

`git-sync-status/Tests/GitSyncStatusCoreTests/CoreTests.swift`:

```swift
import Foundation
import Testing
@testable import GitSyncStatusCore

/// The shape `git-sync status --json` prints (see internal/status/build.go).
let fixture = """
{"at":"2026-10-09T17:20:00-06:00","syncing":true,"problems":1,"repos":[
 {"repo":"git-sync","path":"/Users/me/c/git-sync","state":"syncing","last_sync":"2026-10-09T17:19:00-06:00",
  "running":[{"op":"push","started":"2026-10-09T17:19:58-06:00"}],"problems":[]},
 {"repo":"top_cpu","path":"/Users/me/c/top_cpu","state":"error","last_sync":"2026-10-09T16:10:00-06:00",
  "running":[],"problems":[{"ts":"2026-10-09T16:20:00-06:00","op":"notify","peer":"192.168.1.1","status":"error","msg":"peer 192.168.1.1 receive failed (exit 1)"}]},
 {"repo":"zsh","path":"/Users/me/c/zsh","state":"never","running":[],"problems":[]}
],"pending":[{"kind":"notify","repo":"agents-configs","path":"/Users/me/c/agents-configs","peer":"192.168.1.3","since":"2026-10-09T16:49:36-06:00"}]}
""".replacingOccurrences(of: "\n", with: "")

func decoded() throws -> Snapshot {
    try Snapshot.decode(Data(fixture.utf8))
}

@Test func decodesASnapshot() throws {
    let s = try decoded()
    #expect(s.syncing)
    #expect(s.problems == 1)
    #expect(s.repos.map(\.repo) == ["git-sync", "top_cpu", "zsh"])
    #expect(s.repos[0].state == .syncing)
    #expect(s.repos[0].running.first?.op == "push")
    #expect(s.repos[1].problems.first?.peer == "192.168.1.1")
    #expect(s.repos[2].lastSync == nil)
    #expect(s.pending.first?.peer == "192.168.1.3")
    #expect(s.error == nil)
}

@Test func decodesTheNotInstalledSnapshot() throws {
    let line = #"{"at":"2026-10-09T17:20:00-06:00","syncing":false,"problems":0,"repos":[],"pending":[],"error":"git-sync is not installed: no config at /x"}"#
    let s = try Snapshot.decode(Data(line.utf8))
    #expect(s.error?.contains("not installed") == true)
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `swift test --package-path git-sync-status`
Expected: FAIL to compile: `cannot find 'Snapshot' in scope`.

- [ ] **Step 3: Implement**

`git-sync-status/Sources/GitSyncStatusCore/Snapshot.swift`:

```swift
import Foundation

/// One line of `git-sync status --json --follow`. Mirrors the Go
/// `status.Snapshot`: snake_case keys, whole-second ISO 8601 times, and lists
/// that are always present.
public struct Snapshot: Decodable, Sendable, Equatable {
    public var at: Date
    public var syncing: Bool
    public var problems: Int
    public var repos: [Repo]
    public var pending: [Pending]
    public var error: String?

    public enum State: String, Decodable, Sendable {
        case syncing, error, warn, ok, never
    }

    public struct Repo: Decodable, Sendable, Equatable {
        public var repo: String
        public var path: String
        public var state: State
        public var lastSync: Date?
        public var running: [Running]
        public var problems: [Problem]
    }

    public struct Running: Decodable, Sendable, Equatable {
        public var op: String
        public var peer: String?
        public var started: Date
    }

    public struct Problem: Decodable, Sendable, Equatable {
        public var ts: Date
        public var op: String
        public var peer: String?
        public var status: String
        public var msg: String
    }

    public struct Pending: Decodable, Sendable, Equatable {
        public var kind: String
        public var repo: String
        public var path: String
        public var peer: String?
        public var since: Date
    }

    public static func decode(_ line: Data) throws -> Snapshot {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        d.dateDecodingStrategy = .iso8601
        return try d.decode(Snapshot.self, from: line)
    }
}
```

- [ ] **Step 4: Run the tests**

Run: `swift test --package-path git-sync-status`
Expected: PASS, 2 tests.

- [ ] **Step 5: Commit**

```bash
git add git-sync-status/Package.swift git-sync-status/Sources git-sync-status/Tests
git commit -m "feat(git-sync-status): Swift package and snapshot decoding

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Core rows, relative times and icon state

**Files:**
- Create: `git-sync-status/Sources/GitSyncStatusCore/Rows.swift`
- Test: `git-sync-status/Tests/GitSyncStatusCoreTests/CoreTests.swift` (append)

- [ ] **Step 1: Write the failing tests**

Append to `CoreTests.swift`:

```swift
@Test func mapsReposInOrderAndPendingLast() throws {
    let (repos, pending) = rows(for: try decoded())
    #expect(repos.map(\.name) == ["git-sync", "top_cpu", "zsh"])
    #expect(repos[0].state == "syncing (push)")
    #expect(repos[0].tone == .syncing)
    #expect(repos[1].tone == .error)
    #expect(repos[1].detail == "notify → 192.168.1.1: peer 192.168.1.1 receive failed (exit 1)")
    #expect(repos[2].state == "never synced")
    #expect(repos[2].when == nil)
    #expect(pending.count == 1)
    #expect(pending[0].state == "→ 192.168.1.3")
    #expect(pending[0].path == "/Users/me/c/agents-configs")
    #expect(pending[0].kind == .pending)
}

@Test func everyRowCopiesItsRepoPath() throws {
    let (repos, pending) = rows(for: try decoded())
    #expect((repos + pending).allSatisfy { $0.path.hasPrefix("/Users/me/c/") })
}

@Test func rowIDsAreUnique() throws {
    let (repos, pending) = rows(for: try decoded())
    let ids = (repos + pending).map(\.id)
    #expect(Set(ids).count == ids.count)
}

@Test func agoReadsLikeAHuman() {
    let now = Date(timeIntervalSince1970: 1_000_000)
    #expect(ago(nil, now: now) == "–")
    #expect(ago(now.addingTimeInterval(-5), now: now) == "now")
    #expect(ago(now.addingTimeInterval(-120), now: now).contains("2 min"))
}

@Test func iconStateFollowsTheSnapshot() throws {
    #expect(IconState(nil) == IconState(syncing: false, problem: false))
    #expect(IconState(try decoded()) == IconState(syncing: true, problem: true))
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `swift test --package-path git-sync-status`
Expected: FAIL to compile (`rows`, `ago` and `IconState` are undefined).

- [ ] **Step 3: Implement**

`git-sync-status/Sources/GitSyncStatusCore/Rows.swift`:

```swift
import Foundation

public enum Tone: Sendable, Equatable {
    case ok, error, warn, syncing, muted
}

/// One line of the window's table: a repo, or a queued delivery under
/// "Pending". Clicking any row copies `path`.
public struct Row: Identifiable, Equatable, Sendable {
    public enum Kind: Sendable, Equatable { case repo, pending }

    public var id: String
    public var kind: Kind
    public var name: String
    public var symbol: String
    public var state: String
    public var tone: Tone
    public var when: Date?
    public var detail: String
    public var fullDetail: String
    public var path: String
}

/// The table's rows, in the order git-sync sent them (it already sorts
/// syncing, then problems, then most recent).
public func rows(for s: Snapshot) -> (repos: [Row], pending: [Row]) {
    (s.repos.map(repoRow), s.pending.map(pendingRow))
}

func repoRow(_ r: Snapshot.Repo) -> Row {
    let problems = r.problems.map { "\(label($0.op, $0.peer)): \($0.msg)" }
    let (symbol, state, tone): (String, String, Tone) = switch r.state {
    case .syncing: ("↻", "syncing (\(r.running.first?.op ?? "…"))", .syncing)
    case .error: ("✗", "error", .error)
    case .warn: ("⚠", "warn", .warn)
    case .ok: ("✓", "ok", .ok)
    case .never: ("–", "never synced", .muted)
    }
    return Row(
        id: "repo:" + r.repo, kind: .repo, name: r.repo, symbol: symbol, state: state, tone: tone,
        when: r.lastSync, detail: problems.first ?? "", fullDetail: problems.joined(separator: "\n"),
        path: r.path
    )
}

func pendingRow(_ p: Snapshot.Pending) -> Row {
    let isPush = p.kind == "push"
    let to = isPush ? "remote" : (p.peer ?? "?")
    let why = isPush ? "remote unreachable, will retry" : "\(to) offline, will retry"
    return Row(
        id: "pending:\(p.kind):\(p.peer ?? ""):\(p.repo)", kind: .pending, name: p.repo,
        symbol: "→", state: "→ \(to)", tone: .muted, when: p.since,
        detail: why, fullDetail: why, path: p.path
    )
}

func label(_ op: String, _ peer: String?) -> String {
    guard let peer, !peer.isEmpty else { return op }
    return "\(op) → \(peer)"
}

/// "now", "2 min. ago", "1 hr. ago"; "–" for never.
public func ago(_ date: Date?, now: Date) -> String {
    guard let date else { return "–" }
    if now.timeIntervalSince(date) < 60 { return "now" }
    let f = RelativeDateTimeFormatter()
    f.locale = Locale(identifier: "en_US")
    f.unitsStyle = .abbreviated
    return f.localizedString(for: date, relativeTo: now)
}

/// What the menu bar icon shows: the glyph turning while anything syncs, and a
/// red dot while any repo has a problem. Pending work never sets the dot.
public struct IconState: Equatable, Sendable {
    public var syncing: Bool
    public var problem: Bool

    public init(syncing: Bool, problem: Bool) {
        self.syncing = syncing
        self.problem = problem
    }

    public init(_ s: Snapshot?) {
        self.init(syncing: s?.syncing ?? false, problem: (s?.problems ?? 0) > 0)
    }
}
```

- [ ] **Step 4: Run the tests**

Run: `swift test --package-path git-sync-status`
Expected: PASS, 7 tests.

- [ ] **Step 5: Commit**

```bash
git add git-sync-status/Sources/GitSyncStatusCore git-sync-status/Tests
git commit -m "feat(git-sync-status): table rows, relative times, icon state

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: The app — process feed and launch

**Files:**
- Replace: `git-sync-status/Sources/GitSyncStatus/main.swift`
- Create: `git-sync-status/Sources/GitSyncStatus/AppDelegate.swift`, `git-sync-status/Sources/GitSyncStatus/StatusFeed.swift`

AppKit glue has no unit tests. It's checked by building it here and by the manual check in Task 13.

- [ ] **Step 1: Write the code**

`main.swift`:

```swift
import AppKit

// A menu bar-only app: no Dock icon, no app menu (LSUIElement in the
// bundle's Info.plist says the same for launches through Finder or `open`).
let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
```

`AppDelegate.swift`:

```swift
import AppKit

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let feed = StatusFeed()
    private var statusItem: StatusItemController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = StatusItemController(feed: feed)
        feed.start()
    }

    func applicationWillTerminate(_ notification: Notification) {
        feed.stop()
    }
}
```

For this task only, so it compiles before Task 12 exists, add a temporary stub at the bottom of `AppDelegate.swift`. Task 12 deletes it:

```swift
// Replaced by StatusItemController.swift in the next task.
@MainActor final class StatusItemController {
    init(feed: StatusFeed) {}
}
```

`StatusFeed.swift`:

```swift
import Foundation
import Observation
import GitSyncStatusCore

/// Runs `git-sync status --json --follow` (the installed copy the hooks use)
/// and publishes each snapshot it prints. git-sync is the only reader of
/// ~/.gitsync; this never looks there itself. If the process exits - an
/// ./activate replaced the binary, say - it is started again after 2 s.
@MainActor
@Observable
final class StatusFeed {
    private(set) var snapshot: Snapshot?
    private(set) var updatedAt: Date?
    private(set) var feedError: String?

    /// Called after every change, for the parts of the app that are not
    /// SwiftUI (the menu bar icon).
    @ObservationIgnored var onChange: (() -> Void)?

    @ObservationIgnored private var process: Process?
    @ObservationIgnored private var input: Pipe?
    @ObservationIgnored private var stopped = false

    static let binary = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent(".gitsync/bin/git-sync")

    func start() {
        stopped = false
        launch()
    }

    func stop() {
        stopped = true
        // git-sync status exits on stdin EOF; terminate is the backstop.
        try? input?.fileHandleForWriting.close()
        process?.terminate()
    }

    private func launch() {
        guard !stopped else { return }
        let p = Process()
        p.executableURL = Self.binary
        p.arguments = ["status", "--json", "--follow"]
        let output = Pipe()
        let input = Pipe()
        p.standardOutput = output
        p.standardInput = input
        p.standardError = FileHandle.nullDevice
        p.terminationHandler = { [weak self] _ in
            Task { @MainActor in self?.exited() }
        }
        do {
            try p.run()
        } catch {
            publishError("cannot run \(Self.binary.path): \(error.localizedDescription)")
            relaunchLater()
            return
        }
        process = p
        self.input = input

        let handle = output.fileHandleForReading
        Task.detached { [weak self] in
            do {
                for try await line in handle.bytes.lines {
                    guard let snap = try? Snapshot.decode(Data(line.utf8)) else { continue }
                    await self?.publish(snap)
                }
            } catch {
                // The pipe closed; terminationHandler takes it from here.
            }
        }
    }

    private func publish(_ snap: Snapshot) {
        snapshot = snap
        updatedAt = Date()
        feedError = snap.error
        onChange?()
    }

    private func publishError(_ message: String) {
        feedError = message
        onChange?()
    }

    private func exited() {
        guard !stopped else { return }
        publishError("git-sync status exited, reconnecting…")
        relaunchLater()
    }

    private func relaunchLater() {
        Task { @MainActor [weak self] in
            try? await Task.sleep(for: .seconds(2))
            self?.launch()
        }
    }
}
```

- [ ] **Step 2: Build it**

Run: `swift build --package-path git-sync-status`
Expected: `Build complete!` Warnings are acceptable, errors are not.

- [ ] **Step 3: Commit**

```bash
git add git-sync-status/Sources/GitSyncStatus
git commit -m "feat(git-sync-status): app entry point and status feed

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: The app — animated icon, transient popover, table

**Files:**
- Create: `git-sync-status/Sources/GitSyncStatus/IconRenderer.swift`, `StatusItemController.swift`, `StatusTable.swift`
- Modify: `git-sync-status/Sources/GitSyncStatus/AppDelegate.swift` (delete the temporary stub)

- [ ] **Step 1: Write the code**

Delete the temporary `StatusItemController` stub from `AppDelegate.swift`.

`IconRenderer.swift`:

```swift
import AppKit

/// Composes the menu bar icon: the whole glyph (sync arrows and git mark)
/// rotated by `angle` while something syncs, plus a red dot that never
/// rotates while a repo has a problem. A transparent ring is cleared around
/// the dot so it reads as a badge. The glyph is drawn at display time in the
/// menu bar's own text colour, so it follows light and dark menu bars.
/// Without a problem it is a template image, so the system shades it exactly
/// like its own icons.
@MainActor
struct IconRenderer {
    private let glyph = Bundle.main.image(forResource: "icon")
    private let size = NSSize(width: 22, height: 22)

    func image(angle: CGFloat, problem: Bool) -> NSImage {
        // Run outside the .app (swift run) there is no icon PNG.
        guard let glyph else {
            let fallback = NSImage(systemSymbolName: "arrow.triangle.2.circlepath", accessibilityDescription: "git-sync")
                ?? NSImage(size: size)
            fallback.isTemplate = true
            return fallback
        }
        let image = NSImage(size: size, flipped: false) { rect in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }
            // The badge sits where the SVG puts it: centre (84, 16), dot r 9,
            // cleared ring r 14, on the 90-unit canvas that starts at (5, 5).
            let unit = rect.width / 90
            let centre = CGPoint(x: (84 - 5) * unit, y: rect.height - (16 - 5) * unit)
            func circle(_ r: CGFloat) -> NSRect {
                NSRect(x: centre.x - r * unit, y: centre.y - r * unit, width: 2 * r * unit, height: 2 * r * unit)
            }

            ctx.saveGState()
            ctx.beginTransparencyLayer(auxiliaryInfo: nil)
            ctx.saveGState()
            ctx.translateBy(x: rect.midX, y: rect.midY)
            ctx.rotate(by: angle)
            ctx.translateBy(x: -rect.midX, y: -rect.midY)
            glyph.draw(in: rect)
            ctx.restoreGState()
            NSColor.labelColor.set()
            rect.fill(using: .sourceAtop)
            if problem {
                ctx.setBlendMode(.clear)
                NSBezierPath(ovalIn: circle(14)).fill()
                ctx.setBlendMode(.normal)
            }
            ctx.endTransparencyLayer()
            ctx.restoreGState()
            if problem {
                NSColor.systemRed.set()
                NSBezierPath(ovalIn: circle(9)).fill()
            }
            return true
        }
        image.isTemplate = !problem
        image.accessibilityDescription = "git-sync"
        return image
    }
}
```

`StatusItemController.swift`:

```swift
import AppKit
import SwiftUI
import GitSyncStatusCore

/// The menu bar item: draws the icon from the feed, turns the glyph while
/// something syncs (one turn every 2 s, and no timer at all otherwise), and
/// toggles the window. The window is a transient popover: clicking anywhere
/// else closes it, and so does Esc.
@MainActor
final class StatusItemController: NSObject {
    private let feed: StatusFeed
    private let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
    private let popover = NSPopover()
    private let icon = IconRenderer()
    private var state = IconState(nil)
    private var angle: CGFloat = 0
    private var timer: Timer?
    private var keyMonitor: Any?

    private static let fps = 20.0
    private static let secondsPerTurn = 2.0

    init(feed: StatusFeed) {
        self.feed = feed
        super.init()

        let host = NSHostingController(rootView: StatusTable(feed: feed))
        host.sizingOptions = .preferredContentSize
        popover.contentViewController = host
        popover.behavior = .transient
        popover.animates = false

        if let button = item.button {
            button.target = self
            button.action = #selector(toggle(_:))
        }
        feed.onChange = { [weak self] in self?.refresh() }
        refresh()

        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            guard let self, event.keyCode == 53, self.popover.isShown else { return event } // 53: Esc
            self.popover.performClose(nil)
            return nil
        }
    }

    @objc private func toggle(_ sender: Any?) {
        if popover.isShown {
            popover.performClose(nil)
            return
        }
        guard let button = item.button else { return }
        // An accessory app must activate for the popover to take key events
        // (Esc) and to close on a click elsewhere.
        NSApp.activate()
        popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
        popover.contentViewController?.view.window?.makeKey()
    }

    private func refresh() {
        state = IconState(feed.snapshot)
        if state.syncing {
            if timer == nil {
                timer = Timer.scheduledTimer(withTimeInterval: 1 / Self.fps, repeats: true) { [weak self] _ in
                    MainActor.assumeIsolated { self?.tick() }
                }
            }
        } else {
            timer?.invalidate()
            timer = nil
            angle = 0
        }
        draw()
    }

    private func tick() {
        let step = 2 * CGFloat.pi / CGFloat(Self.fps * Self.secondsPerTurn)
        // The arrows point counter-clockwise, so the turn goes that way too.
        angle = (angle + step).truncatingRemainder(dividingBy: 2 * .pi)
        draw()
    }

    private func draw() {
        item.button?.image = icon.image(angle: angle, problem: state.problem)
        item.button?.toolTip = state.problem ? "git-sync: problems" : (state.syncing ? "git-sync: syncing" : "git-sync")
    }
}
```

`StatusTable.swift`:

```swift
import AppKit
import SwiftUI
import GitSyncStatusCore

private enum Col {
    static let repo: CGFloat = 170
    static let state: CGFloat = 130
    static let when: CGFloat = 90
    static let rowHeight: CGFloat = 24
}

/// The window: one row per synced repo, then the pending queue, then a
/// footer. Clicking a row copies its repo's absolute path.
struct StatusTable: View {
    let feed: StatusFeed
    @State private var copied: String?

    var body: some View {
        let split = feed.snapshot.map { rows(for: $0) } ?? (repos: [], pending: [])
        VStack(alignment: .leading, spacing: 0) {
            header
            Divider()
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0) {
                    if split.repos.isEmpty {
                        Text(feed.snapshot == nil ? "Waiting for git-sync…" : "No repos are selected for syncing.")
                            .foregroundStyle(.secondary)
                            .padding(12)
                    }
                    ForEach(split.repos) { row in line(row) }
                    if !split.pending.isEmpty {
                        Text("Pending")
                            .font(.caption.bold())
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 12)
                            .padding(.top, 10)
                            .padding(.bottom, 4)
                        ForEach(split.pending) { row in line(row) }
                    }
                }
            }
            .frame(height: height(split.repos.count, split.pending.count))
            Divider()
            footer
        }
        .frame(width: 780)
    }

    private func height(_ repos: Int, _ pending: Int) -> CGFloat {
        let body = CGFloat(max(repos, 1)) * Col.rowHeight + (pending > 0 ? 30 + CGFloat(pending) * Col.rowHeight : 0)
        return min(460, body + 8)
    }

    private var header: some View {
        HStack(spacing: 8) {
            Text("Repo").frame(width: Col.repo, alignment: .leading)
            Text("State").frame(width: Col.state, alignment: .leading)
            Text("Last sync").frame(width: Col.when, alignment: .leading)
            Text("Detail").frame(maxWidth: .infinity, alignment: .leading)
        }
        .font(.caption.bold())
        .foregroundStyle(.secondary)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }

    private func line(_ row: Row) -> some View {
        RowView(row: row, copied: copied == row.id) { copy(row) }
    }

    private var footer: some View {
        HStack(spacing: 12) {
            if let error = feed.feedError {
                Text(error).foregroundStyle(.red).lineLimit(1)
            } else if let at = feed.updatedAt {
                Text("Updated \(at.formatted(date: .omitted, time: .standard))").foregroundStyle(.secondary)
            }
            Spacer()
            Text("Click a row to copy its path").foregroundStyle(.tertiary)
            Button("Quit") { NSApp.terminate(nil) }
        }
        .font(.caption)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }

    private func copy(_ row: Row) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(row.path, forType: .string)
        copied = row.id
        Task {
            try? await Task.sleep(for: .seconds(1.2))
            if copied == row.id { copied = nil }
        }
    }
}

private struct RowView: View {
    let row: Row
    let copied: Bool
    let action: () -> Void
    @State private var hover = false

    var body: some View {
        HStack(spacing: 8) {
            Text(row.name)
                .lineLimit(1).truncationMode(.middle)
                .frame(width: Col.repo, alignment: .leading)
            Text("\(row.symbol) \(row.state)")
                .foregroundStyle(color)
                .lineLimit(1)
                .frame(width: Col.state, alignment: .leading)
            TimelineView(.periodic(from: .now, by: 30)) { context in
                Text(ago(row.when, now: context.date)).foregroundStyle(.secondary)
            }
            .frame(width: Col.when, alignment: .leading)
            Text(copied ? "Copied \(row.path)" : row.detail)
                .foregroundStyle(copied ? Color.accentColor : Color.primary)
                .lineLimit(1).truncationMode(.tail)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .font(.system(size: 12))
        .frame(height: Col.rowHeight)
        .padding(.horizontal, 12)
        .background(hover ? Color.primary.opacity(0.08) : Color.clear)
        .contentShape(Rectangle())
        .onHover { hover = $0 }
        .onTapGesture(perform: action)
        .help(row.fullDetail.isEmpty ? row.path : row.fullDetail)
    }

    private var color: Color {
        switch row.tone {
        case .ok: .green
        case .error: .red
        case .warn: .orange
        case .syncing: .accentColor
        case .muted: .secondary
        }
    }
}
```

- [ ] **Step 2: Build it**

Run: `swift build --package-path git-sync-status`
Expected: `Build complete!`

- [ ] **Step 3: Commit**

```bash
git add git-sync-status/Sources/GitSyncStatus
git commit -m "feat(git-sync-status): animated icon, popover window and table

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 13: Build script, first install, manual check

**Files:**
- Replace: `git-sync-status/build`
- Replace: `git-sync-status/.gitignore`

- [ ] **Step 1: Rewrite `git-sync-status/build`**

```sh
#!/bin/sh
# Builds ./git-sync-status.app (next to this script), quits the copy that is
# running, replaces /Applications/git-sync-status.app with it and starts it.
# Tests run first: a failing build never replaces the running app.
# macOS only. See docs/superpowers/specs/2026-10-09-git-sync-status-design.md.
#   ./build
set -eu
cd "$(dirname "$0")"

name=git-sync-status
app=/Applications/$name.app
icon=assets/git-sync-status.svg
out=$name.app

[ "$(uname -s)" = Darwin ] || { echo "$name builds on macOS only" >&2; exit 1; }
command -v rsvg-convert >/dev/null || { echo "rsvg-convert not found: brew install librsvg" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# --- icon ------------------------------------------------------------------
# One glyph, 22pt. The app rotates it whole while syncing and draws the badge
# itself, so the PNG is just the static mark.
rsvg-convert -w 22 -h 22 -o "$tmp/icon.png" "$icon"
rsvg-convert -w 44 -h 44 -o "$tmp/icon@2x.png" "$icon"

# --- test and build --------------------------------------------------------
swift test
swift build -c release --product "$name"
bin=$(swift build -c release --show-bin-path)/$name

# --- bundle ----------------------------------------------------------------
rm -rf "$out"
mkdir -p "$out/Contents/MacOS" "$out/Contents/Resources"
cp "$bin" "$out/Contents/MacOS/$name"
cp "$tmp"/*.png "$out/Contents/Resources/"
cat >"$out/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key><string>$name</string>
  <key>CFBundleIdentifier</key><string>com.grillermo.$name</string>
  <key>CFBundleName</key><string>$name</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$(git rev-parse --short HEAD 2>/dev/null || echo dev)</string>
  <key>LSMinimumSystemVersion</key><string>15.0</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST
codesign --force --sign - "$out" >/dev/null 2>&1

# --- replace and relaunch --------------------------------------------------
# Quit the running copy first: its `git-sync status --follow` child exits on
# stdin EOF once the app is gone.
if pgrep -x "$name" >/dev/null; then
  pkill -x "$name" || true
  i=0
  while pgrep -x "$name" >/dev/null; do
    i=$((i + 1))
    [ "$i" -le 50 ] || { pkill -9 -x "$name" || true; break; }
    sleep 0.1
  done
fi
rm -rf "$app"
ditto "$out" "$app"
open "$app"
echo "installed and started $app"
```

`git-sync-status/.gitignore`:

```
/.build/
/git-sync-status.app/
```

- [ ] **Step 2: Lint and run it**

Run: `shellcheck git-sync-status/build && git-sync-status/build`
Expected: shellcheck is silent. The swift tests pass, and the script ends with `installed and started /Applications/git-sync-status.app`.

- [ ] **Step 3: Check it by hand (requires Task 8's `./activate`, so `~/.gitsync/bin/git-sync` has `status`)**

1. The icon is in the menu bar. It shows the git glyph inside the arrows, it's legible, and its colour matches the other icons.
2. Make a commit in any synced repo (`git commit --allow-empty -m wip` in a scratch synced repo). The whole glyph turns while the push runs and stops upright when it ends. The red dot, if shown, never moves.
3. Click the icon. The window opens under it, with one row per selected repo, and the queued notifies for an offline machine under "Pending".
4. Click a row. The detail cell flashes "Copied …", and `pbpaste` prints that repo's path. Try a pending row too.
5. Click elsewhere and the window closes. Reopen it, press Esc, and it closes.
6. Simulate a problem: `printf '{"ts":"%s","repo":"<a synced rel>","op":"push","status":"error","msg":"test problem"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> ~/.gitsync/activity.jsonl`. Within a second the red dot appears and the row shows `✗ error` with "push: test problem". Then make a commit in that repo: once its push succeeds, the dot clears.
7. Run `pkill -f 'git-sync status --json --follow'`. The footer says "reconnecting…" and recovers within about 2 s.
8. Quit from the footer. `pgrep -f 'git-sync status'` prints nothing.

- [ ] **Step 4: Commit**

```bash
git add git-sync-status/build git-sync-status/.gitignore git-sync-status/assets
git commit -m "feat(git-sync-status): build, install and relaunch script

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 14: Docs

**Files:**
- Modify: `docs/superpowers/specs/2026-10-09-git-sync-status-design.md`, `AGENTS.md`, `README.md`

- [ ] **Step 1: Spec.** Set `Status:` to `implemented`. Fold the eight "Refinements to the spec" from the top of this plan into the matching sections: the run ids into "A repo's state", the offline cases into "Offline is not an error", the first-queued time into the pending paragraph, and the icon, targets and table into "The icon" and "The app". Fix the file layout block: there is no `Sources/GitSyncStatus/Resources/`, and there is a `Sources/GitSyncStatusCore/` and a `Tests/`.

- [ ] **Step 2: AGENTS.md.** Make these changes:
- In "Architecture", the subcommand count and lists: add `status` to the machine-invoked list ("seven are invoked by machines …").
- Add a bullet for `internal/running` after `internal/lock`, and one for `internal/status` after `internal/report`. Each is a short paragraph in the existing style. `running`: one marker file per in-flight push/notify/receive/activate, pid-checked, no lock. `status`: pure `Build` over channels keyed `(repo, op, peer)`, the latest run counts, offline never counts; `Follow` polls a fingerprint every 500 ms and refreshes every minute.
- In the `pending.go` bullet, say that an out-of-reach delivery is logged with status `offline`, and that `ListAllPending` exists.
- In the runtime layout line, add `running/`.
- Add a short "## git-sync-status (macOS menu bar app)" section saying that it lives in `git-sync-status/`, that it reads only `git-sync status --json --follow`, that `swift test --package-path git-sync-status` runs its tests, and that `git-sync-status/build` installs it (the rule already added above).

- [ ] **Step 3: README.md.** Add a short "Menu bar app (macOS)" section: what it shows, `git-sync-status/build` to install, that it runs on demand only, and the icon credits ("syncing" by Gregor Cresnar and "git" from the Noun Project, CC BY 3.0).

- [ ] **Step 4: Commit**

```bash
git add docs AGENTS.md README.md
git commit -m "docs: git-sync-status menu bar app

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Self-review notes

- Spec coverage: icon and animation (Tasks 12–13), the transient window, Esc and click-to-copy on every row (12), the state rules and offline handling (1, 2, 6), in-flight markers (4, 5), `status --json --follow` (7, 8), the pending listing (3), on-demand install into `/Applications` (13), and docs and credits (14). The spec's "Testing" section maps to Tasks 1–10.
- Not covered on purpose (the spec's "Out of scope"): mesh-wide status, notifications, login item, Linux.
- Known limitation, accepted: when one `activate --drain` process runs the same repo twice (a sync landed mid-run), the two runs are separate events with no `Run` id. Each event is its own run, so the later result wins, which is correct.
