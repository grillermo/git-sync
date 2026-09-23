# Multi-Machine Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn git-sync's two-machine peer pair into an N-machine mesh where every machine broadcasts its commits to every other machine, and a machine that is receiving a repo refuses local commits and pushes in that repo until it is done.

**Architecture:** `config.Config` grows a list of `[[peers]]` (migrating the old `peer_host`/`peer_user` pair into a one-entry list). `push` fans out to every peer in parallel; `receive` holds the existing per-repo lock, now carrying an owner record, for the whole sync; two new global hooks (`pre-commit`, `pre-push`) refuse to run while that lock is held. SSH password support is deleted — keys only.

**Tech Stack:** Go 1.26, stdlib plus `BurntSushi/toml v1.6.0`, `bubbletea v1.3.10` / `bubbles v1.0.0` / `lipgloss v1.1.0` (v1 API), plain `go test`.

**Spec:** `docs/superpowers/specs/2026-09-23-multi-machine-sync-design.md`

## Global Constraints

- Go 1.26. No new dependencies; the pinned versions above stay as they are.
- Every filesystem/git test goes through `testutil.NewSandbox(t)`. It uses `t.Setenv`, so **no `t.Parallel()`** in any test that calls it.
- Never touch `~/.gitsync`, `~/.gitconfig` or the real OS keychain in a test.
- `activity` lines stay under `MaxLineLen` (4096); never add a lock around the activity log.
- The remote is resolved by `gitcmd.ResolveRemote(dir, cfg.Remotes())` (`github` → `origin` → sole remote), never from `@{upstream}`.
- Nothing in the sync path may block on a terminal prompt.
- Hooks fail open: `pre-commit`/`pre-push` exit 0 on any git-sync error; `post-commit` never fails a commit.
- `make check` (vet + `gofmt -l .` + `go test ./...`) must pass before a task is done. `go test -race ./internal/activity/...` too when that package is touched.
- After the last task, `make build` so the checked-in `./git-sync` binary matches.
- Run `gofmt -w` on every package you touch.

## Review Focus

These are the failure modes the spec implies but that no task's own happy-path tests would exercise. Each has a test assigned to the task that owns the code.

1. **Shell metacharacters in a peer's host/user/base_dir** reach a remote command string built by `provision`/`repocheck`/`initialsync`. Expected: rejected at config load/validation, never interpolated. → Task 1.
2. **`receive --from <host>` is untrusted input** arriving over ssh; it lands in the lock's owner file and in a message printed to a terminal. Expected: sanitised to a safe subset, never echoed raw. → Task 8.
3. **The same machine listed as its own peer, or a peer listed twice** (a hand-edited config, or an install re-run with `--peer` naming this host). Expected: deduplicated and self-entries dropped, so a machine never notifies itself. → Task 1.
4. **A truncated or unreadable lock owner file** (a process killed mid-write). Expected: `Held` treats it as held-by-unknown rather than panicking, and the stale timer still applies so it cannot block forever. → Task 3.
5. **A config that is missing, unreadable or points at a vanished `base_dir`** while `pre-commit` runs. Expected: the commit is allowed. → Task 4.

---

### Task 1: Peers list in config

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `type Peer struct { Host string; User string; BaseDir string }`
  - `func (p Peer) Target() string` — `"user@host"`
  - `func (p Peer) Validate() error`
  - `Config.Peers []Peer` (toml key `peers`)
  - `func (c Config) PeerList() []Peer` — normalised: migrated, deduplicated, self-entries dropped
  - `func (c Config) WithoutPeer(host string) Config`
  - `Config.PeerHost` / `Config.PeerUser` remain as **migration-only** fields (`toml:"peer_host,omitempty"` / `peer_user,omitempty`), never written by `Marshal`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestLoadMigratesTheOldSinglePeerForm(t *testing.T) {
	sb := testutil.NewSandbox(t)
	writeConfig(t, sb, `base_dir = "`+sb.BaseDir+`"
peer_host = "old.local"
peer_user = "tester"
repos = ["a"]
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	peers := cfg.PeerList()
	if len(peers) != 1 || peers[0].Host != "old.local" || peers[0].User != "tester" {
		t.Fatalf("PeerList() = %+v, want one peer tester@old.local", peers)
	}
}

func TestPeerListDropsDuplicatesAndSelf(t *testing.T) {
	self, _ := os.Hostname()
	cfg := config.Config{Peers: []config.Peer{
		{Host: "b.local", User: "t"},
		{Host: "b.local", User: "t"},
		{Host: self, User: "t"},
	}}
	peers := cfg.PeerList()
	if len(peers) != 1 || peers[0].Host != "b.local" {
		t.Fatalf("PeerList() = %+v, want only b.local", peers)
	}
}

func TestPeerValidateRejectsShellMetacharacters(t *testing.T) {
	for _, p := range []config.Peer{
		{Host: "a.local'; rm -rf ~", User: "t"},
		{Host: "a.local", User: "t;evil"},
		{Host: "a.local", User: "t", BaseDir: "/home/t'/x"},
		{Host: "", User: "t"},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", p)
		}
	}
	if err := (config.Peer{Host: "a.local", User: "t", BaseDir: "/home/t/code"}).Validate(); err != nil {
		t.Errorf("Validate on a plain peer: %v", err)
	}
}

func TestMarshalRoundTripsSeveralPeers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg := config.Config{BaseDir: sb.BaseDir, Repos: []string{"a"}, Peers: []config.Peer{
		{Host: "b.local", User: "t", BaseDir: "/home/t/code"},
		{Host: "c.local", User: "t", BaseDir: "/Users/t/code"},
	}}
	b, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "peer_host") {
		t.Errorf("Marshal still writes the old peer_host key:\n%s", b)
	}
	var got config.Config
	if _, err := toml.Decode(string(b), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Peers) != 2 || got.Peers[1].Host != "c.local" {
		t.Fatalf("round-trip lost peers: %+v", got.Peers)
	}
}
```

Add the helper at the bottom of the test file if it is not already there:

```go
func writeConfig(t *testing.T, sb *testutil.Sandbox, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sb.GitsyncHome, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Peer|Migrates|RoundTrips' -v`
Expected: FAIL — `undefined: config.Peer`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

```go
// Peer is one other machine in the mesh, as reachable from this one.
type Peer struct {
	Host    string `toml:"host"`
	User    string `toml:"user"`
	BaseDir string `toml:"base_dir,omitempty"`
}

func (p Peer) Target() string { return p.User + "@" + p.Host }

// shellUnsafe are characters that would break out of the single-quoted
// remote command strings provision, repocheck and initialsync build. A peer
// is named in those strings, so it is validated once here rather than at
// every interpolation site.
const shellUnsafe = "'\"`$;&|<>()\\ \t\n*?[]{}!#~"

func (p Peer) Validate() error {
	if p.Host == "" || p.User == "" {
		return fmt.Errorf("peer needs both host and user (got %q@%q)", p.User, p.Host)
	}
	for _, f := range []struct{ name, val string }{
		{"host", p.Host}, {"user", p.User},
	} {
		if strings.ContainsAny(f.val, shellUnsafe) {
			return fmt.Errorf("peer %s %q contains characters git-sync will not put in a remote command", f.name, f.val)
		}
	}
	if strings.ContainsAny(p.BaseDir, "'\"`$;&|<>()\\\n*?") {
		return fmt.Errorf("peer base_dir %q contains characters git-sync will not put in a remote command", p.BaseDir)
	}
	return nil
}

// PeerList is the effective set of other machines: the old single-peer form
// migrated in, this machine dropped if it names itself, duplicates removed,
// and invalid entries discarded. Every caller that sshes anywhere uses this.
func (c Config) PeerList() []Peer {
	peers := c.Peers
	if len(peers) == 0 && c.PeerHost != "" {
		peers = []Peer{{Host: c.PeerHost, User: c.PeerUser}}
	}
	self, _ := os.Hostname()
	seen := map[string]bool{}
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		if p.Validate() != nil {
			continue
		}
		if strings.EqualFold(p.Host, self) {
			continue // a machine never notifies itself
		}
		if seen[strings.ToLower(p.Target())] {
			continue
		}
		seen[strings.ToLower(p.Target())] = true
		out = append(out, p)
	}
	return out
}

// WithoutPeer returns a copy with host removed - what each peer's mirrored
// config needs, since a machine never lists itself.
func (c Config) WithoutPeer(host string) Config {
	out := c
	out.Peers = nil
	for _, p := range c.PeerList() {
		if !strings.EqualFold(p.Host, host) {
			out.Peers = append(out.Peers, p)
		}
	}
	return out
}
```

Change the struct fields and `Marshal`:

```go
	// PeerHost and PeerUser are the pre-mesh single-peer form. Read for
	// migration, never written: Marshal emits [[peers]] only.
	PeerHost string `toml:"peer_host,omitempty"`
	PeerUser string `toml:"peer_user,omitempty"`
	// Peers is every other machine in the mesh.
	Peers []Peer `toml:"peers,omitempty"`
```

and in `Marshal`, before encoding:

```go
	c.RemoteNames = c.Remotes()
	c.Peers = c.PeerList()
	c.PeerHost, c.PeerUser = "", ""
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS, including the pre-existing tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/config
git add internal/config
git commit -m "feat(config): peers list with migration from the single-peer form"
```

---

### Task 2: Delete the SSH password machinery

**Files:**
- Delete: `internal/secret/secret.go`, `internal/secret/secret_test.go`, `internal/setup/sshauth.go` (password half), `internal/setup/sshauth_test.go` (password cases)
- Modify: `internal/sshx/sshx.go`, `internal/setup/install.go`, `internal/setup/provision.go`, `cmd/git-sync/main.go`, `cmd/git-sync/stubs.go`, `internal/testutil/testutil.go`
- Test: `internal/setup/sshauth_test.go`, `internal/sshx/` (no test file today; none needed)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `sshx.Command(target, remote string) *exec.Cmd` — unchanged signature, always `BatchMode=yes`; `sshx.Env` is deleted.
  - `setup.Reachable(target string) error` replaces `setup.EnsureAuth`; `setup.IsPeerUnreachable` still reports an unreachable target.

- [ ] **Step 1: Write the failing test**

Replace the password cases in `internal/setup/sshauth_test.go` with:

```go
func TestReachableAcceptsAPeerThatAnswers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	if err := setup.Reachable("tester@peer.example"); err != nil {
		t.Fatalf("Reachable: %v", err)
	}
}

func TestReachableReportsAPeerThatRefuses(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHFailing(255, "Permission denied (publickey).")
	err := setup.Reachable("tester@peer.example")
	if err == nil || !setup.IsPeerUnreachable(err) {
		t.Fatalf("Reachable = %v, want an unreachable error", err)
	}
	if !strings.Contains(err.Error(), "ssh-copy-id") {
		t.Errorf("error should tell the user how to fix it, got %q", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/setup/ -run Reachable -v`
Expected: FAIL — `undefined: setup.Reachable`.

- [ ] **Step 3: Implement**

Rewrite `internal/setup/sshauth.go` down to:

```go
package setup

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/grillermo/git-sync/internal/sshx"
)

// Reachable checks that this machine can ssh to target without a prompt.
// git-sync is key-only: nothing in the sync path has a terminal to answer a
// password prompt with, so a peer that wants one is not usable.
func Reachable(target string) error {
	out, err := sshx.Command(target, "true").CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	_ = errors.As(err, &ee)
	return fmt.Errorf("%w: %s: %s\n         set up a key with: ssh-copy-id %s",
		errPeerUnreachable, target, firstLine(strings.TrimSpace(string(out))), target)
}
```

Simplify `internal/sshx/sshx.go` to:

```go
// Package sshx builds the ssh commands git-sync runs. git-sync is key-only:
// BatchMode=yes everywhere, because nothing in the sync path has a terminal
// that could answer a password prompt.
package sshx

import "os/exec"

func Command(account, remote string) *exec.Cmd {
	return exec.Command("ssh",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		account, remote)
}
```

Then delete, in order, and let the compiler find the callers:

```bash
git rm -r internal/secret
```

- `cmd/git-sync/main.go`: remove the `askpass` and `savepass` cases and the comment above them.
- `cmd/git-sync/stubs.go`: delete `cmdAskpass` and `cmdSavepass`, and the now-unused `bytes`/`secret` imports.
- `internal/setup/install.go`: delete `askpassShimTemplate`, the block that writes it, and the `secret.Delete` call in `Uninstall`. Keep `config.AskpassPath()` in `Uninstall`'s removal list with the comment "left over from the password era; removed on upgrade".
- `internal/setup/provision.go`: delete `setUpTheWayBack`, `confirmYesDefault`, the `In` field on `PeerOptions`, and the `secret`/`bufio`/`bytes` imports that go unused.
- `cmd/git-sync/stubs.go` `cmdInstall`: delete the "connect" stage that called `EnsureAuth`, replacing it with `setup.Reachable(target)` whose error is printed as a warning (install continues).
- `internal/testutil/testutil.go`: delete `StubSSHPassword` and the `GITSYNC_SECRET_BACKEND` line in `NewSandbox`.
- `internal/syncer/e2e_test.go`: delete `GITSYNC_SECRET_BACKEND=file` from the ssh stub script.

- [ ] **Step 4: Run the whole suite**

Run: `make check`
Expected: PASS. Any test that only covered the password path is deleted with it; nothing else should reference `secret`.

Verify nothing survives:

```bash
grep -rn "secret\.\|askpass\|savepass\|GITSYNC_SECRET_BACKEND" --include='*.go' . | grep -v AskpassPath
```
Expected: no output.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal ./cmd
git add -A
git commit -m "refactor: drop ssh password support, keys only"
```

---

### Task 3: Lock owner record, Held and Break

**Files:**
- Modify: `internal/lock/lock.go`
- Test: `internal/lock/lock_test.go`

**Interfaces:**
- Produces:
  - `type Owner struct { From string; Started time.Time; PID int }`
  - `func (o Owner) Age() time.Duration`
  - `func AcquireFrom(rel, from string, timeout time.Duration) (*Lock, error)` (`Acquire` keeps its signature and calls it with `from == ""`)
  - `func (l *Lock) Refresh() error`
  - `func Held(rel string) (Owner, bool)`
  - `func Break(rel string) (Owner, bool, error)`

- [ ] **Step 1: Write the failing tests**

```go
func TestHeldReportsTheOwner(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()

	owner, held := lock.Held("group/proj")
	if !held {
		t.Fatal("Held = false while the lock is taken")
	}
	if owner.From != "laptop.local" {
		t.Errorf("owner.From = %q, want laptop.local", owner.From)
	}
	if owner.Age() > time.Minute {
		t.Errorf("owner.Age() = %v, want a fresh lock", owner.Age())
	}
}

func TestHeldIsFalseWhenNoLockExists(t *testing.T) {
	testutil.NewSandbox(t)
	if _, held := lock.Held("group/proj"); held {
		t.Error("Held = true with no lock")
	}
}

func TestHeldIsFalseForAStaleLock(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()
	old := time.Now().Add(-2 * lock.StaleAfter)
	if err := os.Chtimes(l.Dir(), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("a stale lock must not block anything")
	}
}

func TestRefreshKeepsALockAlive(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()
	old := time.Now().Add(-2 * lock.StaleAfter)
	_ = os.Chtimes(l.Dir(), old, old)
	if err := l.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, held := lock.Held("group/proj"); !held {
		t.Error("a refreshed lock must still be held")
	}
}

// A process killed between mkdir and the owner write leaves a lock with no
// readable owner. It still blocks - something is in there - but it must not
// panic, and the stale timer must still apply.
func TestHeldSurvivesAMissingOrTruncatedOwnerFile(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()
	if err := os.WriteFile(filepath.Join(l.Dir(), "owner"), []byte("{\"from\":"), 0o644); err != nil {
		t.Fatalf("truncate owner: %v", err)
	}
	owner, held := lock.Held("group/proj")
	if !held {
		t.Fatal("a lock with an unreadable owner is still a lock")
	}
	if owner.From != "" {
		t.Errorf("owner.From = %q, want empty for an unreadable record", owner.From)
	}

	old := time.Now().Add(-2 * lock.StaleAfter)
	_ = os.Chtimes(l.Dir(), old, old)
	if _, held := lock.Held("group/proj"); held {
		t.Error("an unreadable owner must not make a lock un-expirable")
	}
}

func TestBreakRemovesTheLockAndReportsWhoHadIt(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	_ = l

	owner, had, err := lock.Break("group/proj")
	if err != nil {
		t.Fatalf("Break: %v", err)
	}
	if !had || owner.From != "laptop.local" {
		t.Fatalf("Break = (%+v, %v), want the previous owner", owner, had)
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("Break left the lock in place")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/lock/ -v`
Expected: FAIL — `undefined: lock.AcquireFrom`.

- [ ] **Step 3: Implement**

In `internal/lock/lock.go`:

```go
// Owner is who holds a lock. Written into the lock directory so the commit
// hooks can tell the user which machine is mid-sync, rather than just
// refusing.
type Owner struct {
	From    string    `json:"from,omitempty"` // the notifying machine
	Started time.Time `json:"started"`
	PID     int       `json:"pid,omitempty"`
}

func (o Owner) Age() time.Duration { return time.Since(o.Started) }

const ownerFile = "owner"

// Acquire takes the lock for rel with no recorded origin.
func Acquire(rel string, timeout time.Duration) (*Lock, error) {
	return AcquireFrom(rel, "", timeout)
}

// AcquireFrom takes the lock and records which machine's notification is
// being processed.
func AcquireFrom(rel, from string, timeout time.Duration) (*Lock, error) {
	l, err := acquireDir(rel, timeout) // the existing mkdir loop, renamed
	if err != nil {
		return nil, err
	}
	l.writeOwner(Owner{From: from, Started: time.Now(), PID: os.Getpid()})
	return l, nil
}

func (l *Lock) writeOwner(o Owner) {
	// Best effort: a lock with no readable owner still blocks, it just
	// cannot name the machine.
	if b, err := json.Marshal(o); err == nil {
		_ = os.WriteFile(filepath.Join(l.dir, ownerFile), b, 0o644)
	}
}

// Refresh restamps the lock so a long sync is not mistaken for a dead one.
func (l *Lock) Refresh() error {
	if l == nil || l.dir == "" {
		return nil
	}
	now := time.Now()
	return os.Chtimes(l.dir, now, now)
}

// Held reports whether rel is locked right now. A stale lock is not held:
// the holder is assumed dead, and nothing should be blocked on it.
func Held(rel string) (Owner, bool) {
	dir := lockDir(rel)
	fi, err := os.Stat(dir)
	if err != nil || time.Since(fi.ModTime()) > StaleAfter {
		return Owner{}, false
	}
	var o Owner
	if b, err := os.ReadFile(filepath.Join(dir, ownerFile)); err == nil {
		_ = json.Unmarshal(b, &o) // an unreadable record leaves a zero Owner
	}
	if o.Started.IsZero() {
		o.Started = fi.ModTime()
	}
	return o, true
}

// Break removes a lock by hand, reporting who held it.
func Break(rel string) (Owner, bool, error) {
	o, held := Held(rel)
	dir := lockDir(rel)
	if _, err := os.Stat(dir); err != nil {
		return o, false, nil
	}
	return o, held, os.RemoveAll(dir)
}
```

Rename the existing `Acquire` body to `acquireDir(rel string, timeout time.Duration) (*Lock, error)` and change `Release` to `os.RemoveAll(l.dir)`, since the directory now has a file in it.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/lock/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/lock
git add internal/lock
git commit -m "feat(lock): owner record, Held, Refresh and Break"
```

---

### Task 4: Blocking hooks and a receiver-aware post-commit

**Files:**
- Modify: `internal/syncer/hook.go`
- Test: `internal/syncer/hook_test.go`

**Interfaces:**
- Consumes: `lock.Held(rel) (lock.Owner, bool)` (Task 3), `config.Config.PeerList()` (Task 1).
- Produces:
  - `func Block(dir string, w io.Writer) int` — the body of `hook pre-commit` and `hook pre-push`; returns the process exit code (1 blocks, 0 allows) and writes the explanation to `w`.
  - `Hook(dir, spawn)` unchanged in signature; it now declines to spawn while the repo's lock is held.

- [ ] **Step 1: Write the failing tests**

```go
func TestBlockRefusesWhileTheRepoIsBeingReceived(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()

	var out bytes.Buffer
	if code := syncer.Block(repo, &out); code != 1 {
		t.Fatalf("Block = %d, want 1 while receiving", code)
	}
	for _, want := range []string{"group/proj", "laptop.local", "git-sync unlock"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("message %q does not mention %q", out.String(), want)
		}
	}
}

func TestBlockAllowsWhenNothingIsBeingReceived(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	var out bytes.Buffer
	if code := syncer.Block(repo, &out); code != 0 {
		t.Fatalf("Block = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("an allowed commit must say nothing, got %q", out.String())
	}
}

func TestBlockAllowsAnUnselectedRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"other/thing"})
	// A lock under this repo's name must not matter: it is not synced.
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	defer l.Release()

	if code := syncer.Block(repo, io.Discard); code != 0 {
		t.Fatalf("Block = %d, want 0 for an unselected repo", code)
	}
}

// Review Focus 5: git-sync being broken must never stop the user committing.
func TestBlockAllowsWhenGitSyncIsBroken(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	// No config at all.
	if code := syncer.Block(repo, io.Discard); code != 0 {
		t.Fatalf("Block = %d with no config, want 0", code)
	}
	// A config that is not parseable.
	if err := os.WriteFile(filepath.Join(sb.GitsyncHome, "config.toml"), []byte("nonsense = ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := syncer.Block(repo, io.Discard); code != 0 {
		t.Fatalf("Block = %d with a corrupt config, want 0", code)
	}
	// A directory that is not a repo at all.
	if code := syncer.Block(sb.Home, io.Discard); code != 0 {
		t.Fatalf("Block = %d outside a repo, want 0", code)
	}
}

func TestBlockAllowsGitSyncsOwnGitCommands(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	defer l.Release()

	t.Setenv("GITSYNC_INTERNAL", "1")
	if code := syncer.Block(repo, io.Discard); code != 0 {
		t.Fatalf("Block = %d for git-sync's own git call, want 0", code)
	}
}

func TestHookDoesNotBroadcastWhileReceiving(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	defer l.Release()

	spawned := false
	if err := syncer.Hook(repo, func(string) error { spawned = true; return nil }); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	if spawned {
		t.Error("a commit made during a receive must not broadcast")
	}
	testutil.AssertEvent(t, activity.OpHook, activity.StatusWarn, "not broadcast")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/syncer/ -run 'Block|DoesNotBroadcast' -v`
Expected: FAIL — `undefined: syncer.Block`.

- [ ] **Step 3: Implement**

Add to `internal/syncer/hook.go`:

```go
// Block is the body of the pre-commit and pre-push hooks: it refuses to let
// the user commit or push in a repo this machine is currently receiving.
//
// It fails open in every other case. git-sync being broken - no config, an
// unreadable one, a repo outside base_dir - must never stop someone
// committing; only a lock that is genuinely held blocks.
func Block(dir string, w io.Writer) int {
	if os.Getenv("GITSYNC_INTERNAL") != "" {
		return 0 // git-sync's own push, not the user's
	}
	rel, ok := selectedRel(dir)
	if !ok {
		return 0
	}
	owner, held := lock.Held(rel)
	if !held {
		return 0
	}
	from := owner.From
	if from == "" {
		from = "another machine"
	}
	fmt.Fprintf(w, "git-sync: %s is receiving changes from %s (started %s ago).\n",
		rel, from, owner.Age().Round(time.Second))
	fmt.Fprintf(w, "Wait a moment and try again. If this is stuck: git-sync unlock %s\n", rel)
	return 1
}

// selectedRel is the repo-identification half of Hook: the relpath of the
// repo containing dir, and whether it is one git-sync syncs. Every error is
// "not ours", because both callers must carry on regardless.
func selectedRel(dir string) (string, bool) {
	cfg, err := config.Load()
	if err != nil {
		return "", false
	}
	root, err := gitcmd.Toplevel(dir)
	if err != nil {
		return "", false
	}
	base := cfg.BaseDir
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	rc := cfg
	rc.BaseDir = base
	rel, err := rc.RepoRel(root)
	if err != nil {
		return "", false
	}
	return rel, cfg.IsSelected(rel)
}
```

In `Hook`, after the `IsSelected` check and before `spawn(rel)`:

```go
	// A commit made while this machine is applying someone else's changes
	// must not broadcast: for this repo we are a receiver, not a
	// broadcaster. --no-verify skips pre-commit but not post-commit, which
	// is exactly the case this catches.
	if owner, held := lock.Held(rel); held {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpHook, Status: activity.StatusWarn,
			Peer: owner.From,
			Msg:  "committed while receiving from " + owner.From + ", not broadcast",
		})
		return nil
	}
```

Refactor `Hook` to use `selectedRel` where it duplicates that logic, keeping its existing "outside base_dir" skip event.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/syncer/ -run 'Hook|Block' -v`
Expected: PASS.

- [ ] **Step 5: Mark git-sync's own git calls as internal**

`Block` honours `GITSYNC_INTERNAL`, but nothing sets it yet. Every git command git-sync runs goes through `gitcmd.Run`, so that is the one place to set it — otherwise git-sync's own `git push` (background push, initial sync) would be refused by git-sync's own `pre-push` hook.

Write the test first, in `internal/gitcmd/gitcmd_test.go`:

```go
func TestRunMarksItsGitCommandsAsInternal(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	// A hook that fails unless it is told this is git-sync's own git call.
	hooks := filepath.Join(sb.Home, "hooks")
	testutil.MkdirAll(t, hooks)
	testutil.WriteFileIn(t, hooks, "pre-commit",
		"#!/bin/sh\n[ -n \"$GITSYNC_INTERNAL\" ] || exit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb.Git(repo, "config", "core.hooksPath", hooks)

	testutil.AppendFileIn(t, repo, "README.md", "x\n")
	if _, err := gitcmd.Run(repo, "commit", "-am", "internal"); err != nil {
		t.Fatalf("gitcmd.Run must set GITSYNC_INTERNAL: %v", err)
	}
}
```

Run: `go test ./internal/gitcmd/ -run Internal -v` → FAIL (exit status 1).

Then in `internal/gitcmd/gitcmd.go`'s `Run`, after `cmd.Dir = dir`:

```go
	// Our own git calls must not be refused by our own pre-commit/pre-push
	// hooks: those exist to stop the *user* committing mid-receive, not to
	// stop git-sync pushing.
	cmd.Env = append(os.Environ(), "GITSYNC_INTERNAL=1")
```

Run: `go test ./internal/gitcmd/ -v` → PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -w ./internal/syncer ./internal/gitcmd
git add internal/syncer internal/gitcmd
git commit -m "feat(syncer): refuse commits and pushes while receiving"
```

---

### Task 5: `hook pre-commit`, `hook pre-push`, and `unlock`

**Files:**
- Modify: `cmd/git-sync/main.go`, `cmd/git-sync/stubs.go`
- Test: `cmd/git-sync/main_test.go` (create if absent; follow `install_wizard_test.go`'s style)

**Interfaces:**
- Consumes: `syncer.Block(dir, w) int` (Task 4), `lock.Break(rel) (lock.Owner, bool, error)` (Task 3), `config.Config.RepoRel` (existing).
- Produces: `cmdUnlock(args []string, stdout, stderr io.Writer) int`; `cmdHook` accepting `post-commit`, `pre-commit` and `pre-push`.

- [ ] **Step 1: Write the failing tests**

```go
func TestHookPreCommitBlocksWhileReceiving(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	defer l.Release()
	testutil.Chdir(t, repo)

	var out, errBuf bytes.Buffer
	if code := run([]string{"hook", "pre-commit"}, &out, &errBuf); code != 1 {
		t.Fatalf("hook pre-commit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String()+out.String(), "laptop.local") {
		t.Errorf("no explanation printed: %q %q", out.String(), errBuf.String())
	}
}

func TestHookPrePushBlocksWhileReceiving(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	defer l.Release()
	testutil.Chdir(t, repo)

	if code := run([]string{"hook", "pre-push"}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("hook pre-push = %d, want 1", code)
	}
}

func TestUnlockClearsTheLockAndNamesTheHolder(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	_ = l
	testutil.Chdir(t, repo)

	var out bytes.Buffer
	if code := run([]string{"unlock"}, &out, io.Discard); code != 0 {
		t.Fatalf("unlock = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "laptop.local") {
		t.Errorf("unlock should name the holder, got %q", out.String())
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("unlock left the lock in place")
	}
}

func TestUnlockOnAnUnlockedRepoIsFine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	testutil.Chdir(t, repo)

	if code := run([]string{"unlock", "group/proj"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("unlock = %d, want 0", code)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/git-sync/ -run 'Hook|Unlock' -v`
Expected: FAIL — `hook pre-commit` returns 2 (usage).

- [ ] **Step 3: Implement**

In `cmd/git-sync/stubs.go`, replace `cmdHook`'s guard and add the new command:

```go
func cmdHook(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: git-sync hook <post-commit|pre-commit|pre-push>")
		return 2
	}
	wd, err := os.Getwd()
	if err != nil {
		return 0 // never block a commit over our own failure
	}
	switch args[0] {
	case "pre-commit", "pre-push":
		return syncer.Block(wd, stderr)
	case "post-commit":
		// ... existing body ...
		return 0
	default:
		fmt.Fprintln(stderr, "usage: git-sync hook <post-commit|pre-commit|pre-push>")
		return 2
	}
}

// cmdUnlock clears a receiver lock left behind by a receive that died.
func cmdUnlock(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "unlock:", err)
		return 1
	}
	rel := ""
	if len(args) > 0 {
		rel = args[0]
	} else {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "unlock:", err)
			return 1
		}
		root, err := gitcmd.Toplevel(wd)
		if err != nil {
			fmt.Fprintln(stderr, "unlock: not inside a git repo; name one: git-sync unlock <repo>")
			return 2
		}
		if rel, err = cfg.RepoRel(root); err != nil {
			fmt.Fprintln(stderr, "unlock:", err)
			return 2
		}
	}
	if err := cfg.ValidateRel(rel); err != nil {
		fmt.Fprintln(stderr, "unlock:", err)
		return 2
	}
	owner, had, err := lock.Break(rel)
	if err != nil {
		fmt.Fprintln(stderr, "unlock:", err)
		return 1
	}
	if !had {
		fmt.Fprintf(stdout, "%s is not locked\n", rel)
		return 0
	}
	from := owner.From
	if from == "" {
		from = "an unknown machine"
	}
	fmt.Fprintf(stdout, "cleared the lock on %s (held by %s since %s)\n",
		rel, from, owner.Started.Format(time.RFC3339))
	_ = activity.Append(activity.Event{
		Repo: rel, Op: activity.OpReceive, Status: activity.StatusWarn,
		Peer: owner.From, Msg: "lock cleared by hand",
	})
	return 0
}
```

In `main.go`, add `case "unlock": return cmdUnlock(args[1:], stdout, stderr)` and a usage line:

```
  git-sync unlock [<repo>]      clear a stuck receive lock (default: this repo)
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./cmd/git-sync/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./cmd/git-sync
git add cmd/git-sync
git commit -m "feat(cli): hook pre-commit/pre-push and the unlock command"
```

---

### Task 6: Install all three hook shims

**Files:**
- Modify: `internal/setup/install.go`
- Test: `internal/setup/install_test.go`

**Interfaces:**
- Produces: `setup.HookNames = []string{"post-commit", "pre-commit", "pre-push"}`; `func hookShim(hookName string) string`.

- [ ] **Step 1: Write the failing test**

```go
func TestInstallWritesAllThreeHookShims(t *testing.T) {
	sb := testutil.NewSandbox(t)
	if err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, NoPeer: true, Self: testutil.WriteScript(t, sb, "fake-git-sync", "#!/bin/sh\n"),
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, name := range []string{"post-commit", "pre-commit", "pre-push"} {
		path := filepath.Join(config.HooksDir(), name)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("hook %s not installed: %v", name, err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("hook %s is not executable (%v)", name, fi.Mode())
		}
		testutil.AssertFileContains(t, path, "hook "+name)
	}
}

func TestUninstallRemovesAllThreeHookShims(t *testing.T) {
	sb := testutil.NewSandbox(t)
	if err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, NoPeer: true, Self: testutil.WriteScript(t, sb, "fake-git-sync", "#!/bin/sh\n"),
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := setup.Uninstall(false, io.Discard); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(config.HooksDir()); !os.IsNotExist(err) {
		t.Errorf("hooks dir survived uninstall: %v", err)
	}
}
```

Match the existing tests' `setup.Options` construction if it differs from the sketch above — read `internal/setup/install_test.go` first and follow it.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/setup/ -run HookShims -v`
Expected: FAIL — `pre-commit` is not installed.

- [ ] **Step 3: Implement**

```go
// HookNames are the git hooks git-sync installs. post-commit broadcasts;
// the other two refuse to run while this machine is receiving that repo.
var HookNames = []string{"post-commit", "pre-commit", "pre-push"}

const hookShimTemplate = `#!/bin/sh
# Installed by git-sync. Removed by 'git-sync uninstall'.
exec %q hook %s
`

func hookShim(binPath, hookName string) string {
	return fmt.Sprintf(hookShimTemplate, binPath, hookName)
}
```

Replace the single-shim write in `Install` with a loop over `HookNames`, keeping `writeFileAtomic` and 0755.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/setup/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/setup
git add internal/setup
git commit -m "feat(setup): install pre-commit and pre-push shims"
```

---

### Task 7: Push fans out to every peer in parallel

**Files:**
- Modify: `internal/syncer/push.go`
- Test: `internal/syncer/push_test.go`

**Interfaces:**
- Consumes: `config.Config.PeerList()` (Task 1).
- Produces: `notifyAll(cfg config.Config, rel, branch string)`; `notifyPeer(p config.Peer, rel, branch string)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPushNotifiesEveryPeer(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"},
		{Host: "c.local", User: "t"},
		{Host: "d.local", User: "t"},
	}, []string{"group/proj"})
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "sync me")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	calls := sb.SSHCalls()
	for _, host := range []string{"b.local", "c.local", "d.local"} {
		if !strings.Contains(calls, host) {
			t.Errorf("peer %s was never notified; calls:\n%s", host, calls)
		}
	}
	events, _ := activity.Read()
	n := 0
	for _, e := range events {
		if e.Op == activity.OpNotify && e.Status == activity.StatusOK {
			n++
		}
	}
	if n != 3 {
		t.Errorf("got %d ok notify events, want one per peer", n)
	}
}

func TestPushKeepsGoingWhenOnePeerIsUnreachable(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "up-one.local", User: "t"},
		{Host: "down.local", User: "t"},
	}, []string{"group/proj"})
	// Exit 255 only for the host whose name contains "down".
	sb.StubSSHScripted(map[string]string{"*down.local*": "exit 255"}, 0)
	testutil.Commit(t, sb, repo, "sync me")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "up-one.local")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusError, "down.local")
}

func TestPushNotifiesPeersConcurrently(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"},
		{Host: "c.local", User: "t"},
		{Host: "d.local", User: "t"},
	}, []string{"group/proj"})
	// Each ssh sleeps 300ms. Sequentially that is 900ms+; in parallel it is
	// one sleep. The bound is generous so a slow machine cannot flake it.
	sb.StubSSHScripted(map[string]string{"*": "sleep 0.3"}, 0)
	testutil.Commit(t, sb, repo, "sync me")

	start := time.Now()
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Errorf("notifications took %v; they are not running in parallel", elapsed)
	}
}
```

Check `StubSSHScripted`'s existing semantics before writing these; if its reply map cannot express "exit 255 for one host", extend it in `internal/testutil/testutil.go` and say so in the commit message.

Add the fixture `SaveConfigWithPeers` to `internal/testutil/testutil.go`, modelled on `SaveConfigWithRepos`:

```go
// SaveConfigWithPeers writes a config naming several machines. repos may be
// nil, in which case every repo under base_dir is selected.
func SaveConfigWithPeers(t *testing.T, sb *Sandbox, peers []config.Peer, repos []string) {
	t.Helper()
	if repos == nil {
		repos = discoverRepos(t, sb)
	}
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: peers, Repos: repos}
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving config: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/syncer/ -run 'NotifiesEvery|KeepsGoing|Concurrently' -v`
Expected: FAIL — only the first peer is notified.

- [ ] **Step 3: Implement**

Replace `notify` in `internal/syncer/push.go`:

```go
// notifyAll tells every other machine to pull what we just pushed. The peers
// are independent: one unreachable machine must not delay or affect the
// others, so they go out concurrently and each gets its own event.
func notifyAll(cfg config.Config, rel, branch string) {
	var wg sync.WaitGroup
	for _, p := range cfg.PeerList() {
		wg.Add(1)
		go func(p config.Peer) {
			defer wg.Done()
			notifyPeer(cfg, p, rel, branch)
		}(p)
	}
	wg.Wait()
}

// notifyPeer asks one peer to run its own receive for this repo.
func notifyPeer(cfg config.Config, p config.Peer, rel, branch string) {
	self, _ := os.Hostname()
	remote := fmt.Sprintf("~/.gitsync/bin/git-sync receive '%s' --from '%s'", rel, sanitizeHost(self))

	cmd := sshx.Command(p.Target(), remote)
	out, err := cmd.CombinedOutput()

	ev := activity.Event{Repo: rel, Op: activity.OpNotify, Branch: branch, Peer: p.Host}
	switch code := exitCode(err); {
	case err == nil:
		ev.Status, ev.Msg = activity.StatusOK, "peer "+p.Host+" synced"
	case code == ExitRepoNotHere:
		ev.Status, ev.Msg = activity.StatusSkip, "peer "+p.Host+" has no copy of this repo, nothing to sync"
	case code == 255:
		ev.Status, ev.Msg = activity.StatusError, "peer "+p.Host+" unreachable"
	default:
		ev.Status = activity.StatusError
		ev.Msg = fmt.Sprintf("peer %s receive failed (exit %d)", p.Host, code)
	}
	_ = activity.Append(ev)
	if err != nil {
		activity.AppendDebug("ssh " + p.Target() + ": " + strings.TrimSpace(string(out)))
	}
}
```

Call `notifyAll(cfg, rel, branch)` where `notify` was called. Add `sanitizeHost` (shared with Task 8; put it in `push.go` and use it from `receive.go`):

```go
// sanitizeHost reduces a hostname to the characters that are safe both in a
// single-quoted remote command and in a message printed to a terminal.
func sanitizeHost(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_':
			return r
		}
		return -1
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
```

Note that the activity log is append-only with atomic sub-`PIPE_BUF` writes, so the concurrent `activity.Append` calls need no lock.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/syncer/ -run 'Push' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/syncer ./internal/testutil
git add internal/syncer internal/testutil
git commit -m "feat(push): notify every peer in parallel"
```

---

### Task 8: Receive records who it is receiving from

**Files:**
- Modify: `internal/syncer/receive.go`, `cmd/git-sync/stubs.go` (`cmdReceive`)
- Test: `internal/syncer/receive_test.go`

**Interfaces:**
- Consumes: `lock.AcquireFrom`, `(*lock.Lock).Refresh` (Task 3), `sanitizeHost` (Task 7).
- Produces: `func Receive(rel, from string) int` — **signature change**; `cmdReceive` parses `--from`.

- [ ] **Step 1: Write the failing tests**

```go
func TestReceiveRecordsTheNotifyingMachineInTheLock(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from the peer")

	// Observe the lock from inside the sync: a pre-commit hook firing at
	// this moment is exactly what must see the owner.
	seen := make(chan lock.Owner, 1)
	go func() {
		for i := 0; i < 200; i++ {
			if o, held := lock.Held("group/proj"); held {
				seen <- o
				return
			}
			time.Sleep(time.Millisecond)
		}
		close(seen)
	}()

	if code := syncer.Receive("group/proj", "laptop.local"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	select {
	case o, ok := <-seen:
		if !ok {
			t.Skip("the sync finished before the watcher sampled the lock")
		}
		if o.From != "laptop.local" {
			t.Errorf("lock owner From = %q, want laptop.local", o.From)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher never reported")
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("the lock outlived the receive")
	}
	_ = repo
}

// Review Focus 2: --from arrives over ssh from another machine.
func TestReceiveSanitisesTheFromHost(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")

	if code := syncer.Receive("group/proj", "evil`whoami`.local; rm -rf /"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	events, _ := activity.Read()
	for _, e := range events {
		if strings.ContainsAny(e.Peer+e.Msg, "`;$") {
			t.Errorf("unsanitised host reached the log: %+v", e)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/syncer/ -run 'Receive(Records|Sanitises)' -v`
Expected: FAIL — `too many arguments in call to syncer.Receive`.

- [ ] **Step 3: Implement**

In `internal/syncer/receive.go`:

```go
// Receive applies whatever a peer just pushed. from names the notifying
// machine; it arrives over ssh, so it is sanitised before it is recorded or
// shown. It is used only for the lock's owner record and the message the
// blocking hooks print.
func Receive(rel, from string) int {
	from = sanitizeHost(from)
	// ... unchanged validation ...
	l, err := lock.AcquireFrom(rel, from, lockTimeout())
	// ... unchanged busy handling ...
	defer l.Release()

	// A large fetch can outlast StaleAfter. While that happens the lock must
	// keep looking alive, or a commit hook would decide the holder is dead
	// and let a commit through mid-merge.
	stop := heartbeat(l)
	defer stop()

	return syncRepo(cfg, rel, dir)
}

// heartbeat restamps the lock every minute until the returned function is
// called.
func heartbeat(l *lock.Lock) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = l.Refresh()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
```

In `cmdReceive`:

```go
func cmdReceive(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("receive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "the machine that sent this notification")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: git-sync receive <repo> [--from <host>]")
		return 2
	}
	return syncer.Receive(fs.Arg(0), *from)
}
```

`flag` stops at the first non-flag argument, so `receive <repo> --from <host>` needs the repo re-parsed: put `--from` **before** the repo in the command `push` builds, or call `fs.Parse` on a reordered slice. Simplest and what Task 7's command string already assumes — reorder in `cmdReceive`:

```go
	// `receive <repo> --from <host>`: flag.Parse stops at <repo>, so pull a
	// leading non-flag argument out before parsing.
	var rest []string
	repo := ""
	for _, a := range args {
		if repo == "" && !strings.HasPrefix(a, "-") {
			repo = a
			continue
		}
		rest = append(rest, a)
	}
```

then parse `rest` and use `repo`.

Update every existing `syncer.Receive(rel)` call site (tests included) to pass a `from`.

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/syncer/ ./cmd/git-sync/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/syncer ./cmd/git-sync
git add internal/syncer cmd/git-sync
git commit -m "feat(receive): record the notifying machine on the lock"
```

---

### Task 9: Provision each peer with a mesh config

**Files:**
- Modify: `internal/setup/provision.go`
- Test: `internal/setup/provision_test.go`

**Interfaces:**
- Consumes: `config.Config.WithoutPeer`, `config.Peer` (Task 1), `setup.HookNames`, `hookShim` (Task 6).
- Produces: `PeerOptions.Peer config.Peer` replaces `Cfg.PeerHost`/`PeerUser` as the target; `ProvisionPeer` writes all three shims and a mesh config.

- [ ] **Step 1: Write the failing tests**

```go
func TestProvisionWritesAMeshConfigExcludingThePeerItself(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"*uname*": "Darwin arm64",
		"*HOME*":  "/home/tester",
	}, 0)

	cfg := config.Config{BaseDir: sb.BaseDir, Repos: []string{"a"}, Peers: []config.Peer{
		{Host: "b.local", User: "t"},
		{Host: "c.local", User: "t"},
	}}
	err := setup.ProvisionPeer(setup.PeerOptions{
		Cfg: cfg, Peer: config.Peer{Host: "b.local", User: "t"},
		Self: testutil.WriteScript(t, sb, "fake", "#!/bin/sh\n"),
		SelfHost: "a.local", SelfUser: "t", Out: io.Discard,
	})
	if err != nil {
		t.Fatalf("ProvisionPeer: %v", err)
	}
	written := sb.SSHStdin(t, "config.toml")
	if strings.Contains(written, `host = "b.local"`) {
		t.Errorf("b.local's own config lists itself:\n%s", written)
	}
	for _, want := range []string{`host = "c.local"`, `host = "a.local"`} {
		if !strings.Contains(written, want) {
			t.Errorf("b.local's config is missing %s:\n%s", want, written)
		}
	}
}

func TestProvisionWritesAllThreeHookShims(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"*uname*": "Darwin arm64",
		"*HOME*":  "/home/tester",
	}, 0)
	cfg := config.Config{BaseDir: sb.BaseDir, Repos: []string{"a"},
		Peers: []config.Peer{{Host: "b.local", User: "t"}}}
	if err := setup.ProvisionPeer(setup.PeerOptions{
		Cfg: cfg, Peer: cfg.Peers[0],
		Self:     testutil.WriteScript(t, sb, "fake", "#!/bin/sh\n"),
		SelfHost: "a.local", SelfUser: "t", Out: io.Discard,
	}); err != nil {
		t.Fatalf("ProvisionPeer: %v", err)
	}
	calls := sb.SSHCalls()
	for _, name := range []string{"hooks/post-commit", "hooks/pre-commit", "hooks/pre-push"} {
		if !strings.Contains(calls, name) {
			t.Errorf("%s was never written on the peer; calls:\n%s", name, calls)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/setup/ -run Provision -v`
Expected: FAIL — `unknown field Peer in struct literal`.

- [ ] **Step 3: Implement**

In `internal/setup/provision.go`:

- Add `Peer config.Peer` to `PeerOptions` and use `o.Peer.Target()` instead of `Target(o.Cfg)`.
- Replace the mirrored-config block:

```go
	// The peer's config: the same repos, its own base_dir, and the rest of
	// the mesh - every machine except itself, plus us.
	peerCfg := o.Cfg.WithoutPeer(o.Peer.Host)
	peerCfg.BaseDir = o.peerBase(peerHome)
	peerCfg.Repos = o.Cfg.Repos
	peerCfg.Peers = append(peerCfg.Peers, config.Peer{
		Host: o.SelfHost, User: o.SelfUser, BaseDir: o.Cfg.BaseDir,
	})
```

Note: `Marshal` calls `PeerList()`, which drops any entry whose host equals *this* machine's hostname. That is correct for our own file but wrong for a peer's, so add an explicit variant used here:

```go
// MarshalFor produces the config bytes for the machine named host: the same
// as Marshal, except self-filtering uses that machine's name rather than
// this one's.
func (c Config) MarshalFor(host string) ([]byte, error)
```

Implement `MarshalFor` in `internal/config/config.go` by factoring `Marshal`'s body to take the self-host, with `Marshal` passing `os.Hostname()`. Add a config test:

```go
func TestMarshalForKeepsThisMachineInAPeersConfig(t *testing.T) {
	self, _ := os.Hostname()
	cfg := config.Config{BaseDir: "/x", Peers: []config.Peer{
		{Host: self, User: "t"}, {Host: "b.local", User: "t"},
	}}
	b, err := cfg.MarshalFor("b.local")
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	if !strings.Contains(string(b), self) {
		t.Errorf("b.local's config must list %s:\n%s", self, b)
	}
	if strings.Contains(string(b), `host = "b.local"`) {
		t.Errorf("b.local's config must not list itself:\n%s", b)
	}
}
```

- Replace the single hook write with a loop over `HookNames`, each `cat > … .tmp && chmod +x && mv`.
- `Probe` and `checkSamePlatform` are unchanged.
- Delete the `fmt.Fprintf(o.Out, ...)` lines that name `o.Cfg.PeerHost`; print `o.Peer.Host` instead.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/setup/ ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/setup ./internal/config
git add internal/setup internal/config
git commit -m "feat(setup): provision peers with mesh configs and all hooks"
```

---

### Task 10: Pairwise key check

**Files:**
- Create: `internal/setup/keycheck.go`, `internal/setup/keycheck_test.go`
- Modify: none yet (wired into install in Task 12)

**Interfaces:**
- Consumes: `config.Peer` (Task 1), `sshx.Command` (Task 2).
- Produces:
  - `type KeyResult struct { From, To config.Peer; OK bool; Err string }`
  - `func CheckKeys(self config.Peer, peers []config.Peer) []KeyResult`
  - `func RenderKeyChecks(w io.Writer, results []KeyResult) int` — returns the number of failing pairs.

- [ ] **Step 1: Write the failing tests**

```go
func TestCheckKeysProbesEveryOrderedPair(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := config.Peer{Host: "a.local", User: "t"}
	peers := []config.Peer{{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"}}

	results := setup.CheckKeys(self, peers)
	// a->b, a->c, b->c, c->b: four ordered pairs, and never a self-pair.
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4: %+v", len(results), results)
	}
	for _, r := range results {
		if r.From.Host == r.To.Host {
			t.Errorf("a machine was checked against itself: %+v", r)
		}
		if !r.OK {
			t.Errorf("all pairs should pass with a working ssh: %+v", r)
		}
	}
}

func TestCheckKeysReportsAFailingPair(t *testing.T) {
	sb := testutil.NewSandbox(t)
	// The outer ssh succeeds; the inner one (run on b, targeting c) fails.
	sb.StubSSHScripted(map[string]string{"*c.local*": "exit 255"}, 0)
	self := config.Peer{Host: "a.local", User: "t"}
	peers := []config.Peer{{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"}}

	results := setup.CheckKeys(self, peers)
	var failed []string
	for _, r := range results {
		if !r.OK {
			failed = append(failed, r.From.Host+"->"+r.To.Host)
		}
	}
	if len(failed) == 0 {
		t.Fatal("a failing target must produce a failing pair")
	}

	var out bytes.Buffer
	n := setup.RenderKeyChecks(&out, results)
	if n != len(failed) {
		t.Errorf("RenderKeyChecks = %d, want %d", n, len(failed))
	}
	if !strings.Contains(out.String(), "ssh-copy-id") {
		t.Errorf("the report must say how to fix it:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/setup/ -run CheckKeys -v`
Expected: FAIL — `undefined: setup.CheckKeys`.

- [ ] **Step 3: Implement**

`internal/setup/keycheck.go`:

```go
package setup

import (
	"fmt"
	"io"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/sshx"
)

// KeyResult is one ordered pair: can From ssh to To without a prompt?
type KeyResult struct {
	From config.Peer
	To   config.Peer
	OK   bool
	Err  string
}

// keyCheckMarker opens the remote command, so it is identifiable in the debug
// log and in the ssh stub's recorded calls.
const keyCheckMarker = "# git-sync-key-check"

// CheckKeys probes every ordered pair of machines in the mesh. git-sync is
// key-only and every machine notifies every other, so a missing key between
// two *peers* - a pair this machine never exercises itself - would otherwise
// only show up later as a failing notify nobody is watching.
func CheckKeys(self config.Peer, peers []config.Peer) []KeyResult {
	var out []KeyResult
	for _, p := range peers {
		out = append(out, probeKey(self, p, func(to config.Peer) error {
			return ssh(to.Target(), keyCheckMarker+"\ntrue")
		}))
	}
	for _, from := range peers {
		for _, to := range peers {
			if strings.EqualFold(from.Host, to.Host) {
				continue
			}
			to := to
			out = append(out, probeKey(from, to, func(to config.Peer) error {
				// Run the check *on* `from`, targeting `to`.
				return ssh(from.Target(), fmt.Sprintf(
					"%s\nssh -o BatchMode=yes -o ConnectTimeout=5 %s true",
					keyCheckMarker, to.Target()))
			}))
		}
	}
	return out
}

func probeKey(from, to config.Peer, run func(config.Peer) error) KeyResult {
	r := KeyResult{From: from, To: to, OK: true}
	if err := run(to); err != nil {
		r.OK, r.Err = false, firstLine(err.Error())
	}
	return r
}

// RenderKeyChecks prints the pairs that cannot connect and returns how many
// there were. Install warns on these rather than refusing: the rest of the
// mesh is still worth setting up, and those pairs simply will not sync until
// a key is added.
func RenderKeyChecks(w io.Writer, results []KeyResult) int {
	var bad []KeyResult
	for _, r := range results {
		if !r.OK {
			bad = append(bad, r)
		}
	}
	if len(bad) == 0 {
		return 0
	}
	fmt.Fprintf(w, "\n%d machine pairs cannot ssh to each other:\n", len(bad))
	for _, r := range bad {
		fmt.Fprintf(w, "  %s -> %s: %s\n", r.From.Host, r.To.Host, r.Err)
		fmt.Fprintf(w, "      fix on %s with: ssh-copy-id %s\n", r.From.Host, r.To.Target())
	}
	fmt.Fprintln(w, "  those directions will not sync until a key is in place; the rest will.")
	return len(bad)
}
```

`ssh(target, remote string) error` above is the package's existing unexported helper in `provision.go`; reuse it rather than adding another.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/setup/ -run CheckKeys -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/setup
git add internal/setup
git commit -m "feat(setup): pairwise ssh key check across the mesh"
```

---

### Task 11: Initial sync across N machines

**Files:**
- Modify: `internal/setup/initialsync.go`
- Test: `internal/setup/initialsync_test.go`

**Interfaces:**
- Consumes: `config.Peer`, `config.Config.PeerList()` (Task 1).
- Produces:
  - `RepoSync.There []PeerPos` replaces `RepoSync.There SyncPos`, where `type PeerPos struct { Peer config.Peer; Pos SyncPos }`.
  - `func MeasureSync(cfg config.Config, peers []PeerTarget, repos []string) ([]RepoSync, error)` where `type PeerTarget struct { Peer config.Peer; BaseDir string }`.
  - `func ApplySync(cfg config.Config, peers []PeerTarget, repos []RepoSync) []RepoSync`
  - `RenderSyncPlan` / `RenderSyncResult` keep their shape but print one line per machine.

- [ ] **Step 1: Write the failing tests**

Rewrite the existing two-machine tests to the new shape, and add:

```go
func TestApplySyncPushesTheFirstMachineThatIsAhead(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.Commit(t, sb, repo, "only here")
	// Two peers, both level with the remote and both behind us.
	sb.StubSSHScripted(map[string]string{"*": "true"}, 0)
	cfg := config.Config{BaseDir: sb.BaseDir, Repos: []string{"group/proj"}, Peers: []config.Peer{
		{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"},
	}}
	peers := []setup.PeerTarget{
		{Peer: cfg.Peers[0], BaseDir: "/home/t/code"},
		{Peer: cfg.Peers[1], BaseDir: "/home/t/code"},
	}

	measured, err := setup.MeasureSync(cfg, peers, cfg.Repos)
	if err != nil {
		t.Fatalf("MeasureSync: %v", err)
	}
	if len(measured) != 1 || len(measured[0].There) != 2 {
		t.Fatalf("expected one repo measured on two peers, got %+v", measured)
	}
	setup.ApplySync(cfg, peers, measured)

	// Our commit reached the shared remote, which is the whole point.
	out := sb.Git(repo, "log", "--oneline", "origin/main", "-1")
	if !strings.Contains(out, "only here") {
		t.Errorf("the ahead machine did not push: %s", out)
	}
}

func TestRenderSyncPlanNamesEachMachine(t *testing.T) {
	repos := []setup.RepoSync{{
		Rel:  "group/proj",
		Here: setup.SyncPos{Branch: "main", Remote: "origin", Ahead: 1},
		There: []setup.PeerPos{
			{Peer: config.Peer{Host: "b.local"}, Pos: setup.SyncPos{Branch: "main", Remote: "origin", Behind: 1}},
			{Peer: config.Peer{Host: "c.local"}, Pos: setup.SyncPos{Branch: "main", Remote: "origin", Ahead: 2}},
		},
	}}
	var out bytes.Buffer
	if !setup.RenderSyncPlan(&out, repos) {
		t.Fatal("RenderSyncPlan said there was nothing to do")
	}
	for _, want := range []string{"b.local", "c.local", "group/proj"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan does not mention %q:\n%s", want, out.String())
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/setup/ -run Sync -v`
Expected: FAIL — `MeasureSync` has the old signature.

- [ ] **Step 3: Implement**

Change the types:

```go
// PeerTarget is one machine to measure, with its own sync root.
type PeerTarget struct {
	Peer    config.Peer
	BaseDir string
}

// PeerPos is one machine's position for one repo.
type PeerPos struct {
	Peer config.Peer
	Pos  SyncPos
}

// RepoSync is one selected repo measured on every machine.
type RepoSync struct {
	Rel   string
	Here  SyncPos
	There []PeerPos
}
```

`MeasureSync` runs the existing `syncMeasureScript` once per peer (sequentially — this is install, not the hot path) and folds each answer into `There`.

`blocked` becomes:

```go
// blocked reports whether push and fast-forward cannot fix this repo, so the
// user has to. Two machines ahead is not blocked: the first pushes, and the
// others are reported afterwards by the re-measure.
func (r RepoSync) blocked() bool {
	if !r.Here.ok() {
		return true
	}
	for _, p := range r.There {
		if !p.Pos.ok() || p.Pos.Branch != r.Here.Branch {
			return true
		}
	}
	return false
}
```

`ApplySync` keeps its three passes, generalised:

```go
// ApplySync repairs each repo: exactly one machine publishes - the first that
// is purely ahead, this machine before any peer - and every other machine
// then fast-forwards onto it. A second ahead machine is left alone and
// reported by the re-measure: pushing both would be a merge decision, and
// that is the user's.
func ApplySync(cfg config.Config, peers []PeerTarget, repos []RepoSync) []RepoSync {
	notes := map[string]string{}

	// Pass 1: the one publisher per repo. Repos are grouped by the machine
	// that will push them, so each peer needs at most one round trip.
	pushHere := []RepoSync{}
	pushThere := map[string][]RepoSync{} // keyed by peer host
	for _, r := range repos {
		if r.blocked() {
			continue
		}
		if r.Here.canPush() {
			pushHere = append(pushHere, r)
			continue
		}
		for _, pp := range r.There {
			if pp.Pos.canPush() {
				pushThere[pp.Peer.Host] = append(pushThere[pp.Peer.Host], r)
				break // the first ahead machine only
			}
		}
	}
	for _, r := range pushHere {
		_, _ = gitcmd.Push(cfg.RepoPath(r.Rel), r.Here.Remote, r.Here.Branch)
	}
	for _, pt := range peers {
		rs := pushThere[pt.Peer.Host]
		if len(rs) == 0 {
			continue
		}
		// Best effort: the re-measure below is what the user is shown, so an
		// ssh failure here surfaces as "still not level", never as a false
		// claim of success.
		_, _ = sshOut(pt.Peer.Target(), syncApplyScript(pt.BaseDir, relsOf(rs), cfg.Remotes()))
	}

	// Pass 2: everyone else lands what was just published. Every peer is
	// asked about every unblocked repo - the remote script already no-ops on
	// a repo with nothing to fast-forward.
	var landable []RepoSync
	for _, r := range repos {
		if !r.blocked() {
			landable = append(landable, r)
		}
	}
	for _, pt := range peers {
		if len(landable) == 0 {
			break
		}
		_, _ = sshOut(pt.Peer.Target(), syncApplyScript(pt.BaseDir, relsOf(landable), cfg.Remotes()))
	}
	for _, r := range landable {
		dir := cfg.RepoPath(r.Rel)
		if r.Here.Remote == "" || r.Here.Branch == "" {
			continue
		}
		if err := gitcmd.Fetch(dir, r.Here.Remote); err != nil {
			continue
		}
		ahead, behind, err := gitcmd.AheadBehind(dir, r.Here.Remote, r.Here.Branch)
		if err != nil || behind == 0 || ahead > 0 {
			continue // nothing to land, or diverged - never merged automatically
		}
		if note := landHere(dir, r.Here.Remote, r.Here.Branch); note != "" {
			notes[r.Rel] = note
		}
	}

	// Pass 3: re-measure, so the user is shown what happened rather than what
	// was intended, with the warnings that survive a successful sync.
	final, _ := MeasureSync(cfg, peers, relsOf(repos))
	for i := range final {
		if n, ok := notes[final[i].Rel]; ok {
			final[i].Here.Note = n
		}
	}
	return final
}
```

`landHere`, `relsOf`, `syncApplyScript` and `syncMeasureScript` are unchanged — only their callers move.

`RenderSyncPlan(w io.Writer, repos []RepoSync) bool` drops its `peerHost` argument and prints one indented line per machine:

```
  group/proj
      here:    1 ahead of origin/main
      b.local: 1 behind origin/main
      c.local: 2 ahead of origin/main  (left alone: two machines are ahead)
```

`RenderSyncResult` follows the same shape.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/setup/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/setup
git add internal/setup
git commit -m "feat(setup): level every machine in the mesh at install"
```

---

### Task 12: Install and uninstall across the mesh

**Files:**
- Modify: `cmd/git-sync/stubs.go`, `internal/setup/install.go`
- Test: `cmd/git-sync/install_wizard_test.go`, `internal/setup/install_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1, 6, 9, 10, 11.
- Produces:
  - `cmdInstall` accepting repeatable `--peer user@host[:base_dir]` (with `--peer-host`/`--peer-user` kept as a single-peer alias).
  - `setup.Options.Peers []config.Peer` replaces `PeerHost`/`PeerUser`.
  - `setup.Uninstall(purge bool, out io.Writer) error` unchanged in signature, plus `setup.UninstallMesh(cfg config.Config, purge bool, out io.Writer) error`.
  - `cmdUninstall` gains `--local` (this machine only).

- [ ] **Step 1: Write the failing tests**

```go
func TestInstallAcceptsSeveralPeers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	sb.StubSSHScripted(map[string]string{
		"*uname*": "Darwin arm64",
		"*HOME*":  "/home/t",
	}, 0)

	code := run([]string{"install", "--peer", "t@b.local", "--peer", "t@c.local",
		"--all", "--no-initial-sync", sb.BaseDir}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("install = %d, want 0", code)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.PeerList()) != 2 {
		t.Fatalf("config has %d peers, want 2: %+v", len(cfg.PeerList()), cfg.Peers)
	}
}

func TestInstallParsesAPeerBaseDirSuffix(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	sb.StubSSHScripted(map[string]string{"*uname*": "Darwin arm64", "*HOME*": "/home/t"}, 0)

	if code := run([]string{"install", "--peer", "t@b.local:/srv/code", "--all",
		"--no-initial-sync", sb.BaseDir}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("install = %d, want 0", code)
	}
	cfg, _ := config.Load()
	if cfg.PeerList()[0].BaseDir != "/srv/code" {
		t.Errorf("peer base_dir = %q, want /srv/code", cfg.PeerList()[0].BaseDir)
	}
}

func TestUninstallRemovesEveryMachine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"},
	}, []string{})
	sb.StubSSH(0)

	var out bytes.Buffer
	if code := run([]string{"uninstall"}, &out, io.Discard); code != 0 {
		t.Fatalf("uninstall = %d, want 0", code)
	}
	calls := sb.SSHCalls()
	for _, host := range []string{"b.local", "c.local"} {
		if !strings.Contains(calls, host) {
			t.Errorf("%s was never uninstalled; calls:\n%s", host, calls)
		}
	}
	if !strings.Contains(calls, "uninstall --local") {
		t.Errorf("peers must be uninstalled with --local:\n%s", calls)
	}
}

func TestUninstallReportsAnUnreachableMachine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "gone.local", User: "t"}}, []string{})
	sb.StubSSHFailing(255, "no route to host")

	var out bytes.Buffer
	if code := run([]string{"uninstall"}, &out, io.Discard); code != 0 {
		t.Fatalf("uninstall = %d, want 0 (this machine still uninstalls)", code)
	}
	if !strings.Contains(out.String(), "gone.local") ||
		!strings.Contains(out.String(), "git-sync uninstall") {
		t.Errorf("must tell the user to clean up by hand:\n%s", out.String())
	}
}

func TestUninstallLocalDoesNotTouchPeers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "b.local", User: "t"}}, []string{})
	sb.StubSSH(0)

	if code := run([]string{"uninstall", "--local"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("uninstall --local = %d, want 0", code)
	}
	if strings.Contains(sb.SSHCalls(), "b.local") {
		t.Errorf("--local must not ssh anywhere:\n%s", sb.SSHCalls())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/git-sync/ -run 'Install|Uninstall' -v`
Expected: FAIL — `--peer` is not a flag.

- [ ] **Step 3: Implement**

In `cmd/git-sync/stubs.go`:

```go
// peerFlag collects repeatable --peer user@host[:base_dir] values.
type peerFlag []config.Peer

func (f *peerFlag) String() string { return "" }

func (f *peerFlag) Set(s string) error {
	user, rest, ok := strings.Cut(s, "@")
	if !ok || user == "" || rest == "" {
		return fmt.Errorf("want user@host[:base_dir], got %q", s)
	}
	host, base, _ := strings.Cut(rest, ":")
	p := config.Peer{User: user, Host: host, BaseDir: base}
	if err := p.Validate(); err != nil {
		return err
	}
	*f = append(*f, p)
	return nil
}
```

`cmdInstall` then:

1. Parses `--peer` (repeatable), plus the existing `--peer-host`/`--peer-user` pair folded into one `config.Peer`.
2. Merges with `config.Load()`'s existing peers; prompts only when the result is empty.
3. Calls `setup.Reachable` per peer, collecting the reachable ones.
4. Calls `setup.CheckKeys` + `setup.RenderKeyChecks` and prints the table (warning only).
5. Runs the repo check per reachable peer (Task 13 wires `checkPeer` into a loop over peers).
6. Calls `setup.Install(setup.Options{... Peers: peers ...})`, which provisions each peer via `ProvisionPeer` with `Peer:` set.
7. Runs `levelRepos` with all peer targets unless `--no-initial-sync`.

In `internal/setup/install.go`, `Options.Peers []config.Peer` replaces the two string fields; the provisioning block becomes a loop, reporting per peer and treating `IsPeerUnreachable` as a warning as it does today.

Add `UninstallMesh`:

```go
// UninstallMesh removes git-sync from every machine in the mesh, this one
// last: a peer is reachable only while our own config still names it.
func UninstallMesh(cfg config.Config, purge bool, out io.Writer) error {
	var unreachable []config.Peer
	for _, p := range cfg.PeerList() {
		cmd := "~/.gitsync/bin/git-sync uninstall --local"
		if purge {
			cmd += " --purge"
		}
		if err := ssh(p.Target(), cmd); err != nil {
			unreachable = append(unreachable, p)
			continue
		}
		fmt.Fprintf(out, "uninstalled git-sync on %s\n", p.Host)
	}
	if err := Uninstall(purge, out); err != nil {
		return err
	}
	for _, p := range unreachable {
		fmt.Fprintf(out, "WARNING: could not reach %s; run there by hand: git-sync uninstall\n", p.Host)
	}
	return nil
}
```

`cmdUninstall` gains `--local` and calls `Uninstall` directly for it, `UninstallMesh` otherwise (falling back to `Uninstall` when there is no config).

- [ ] **Step 4: Run to verify they pass**

Run: `make check`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./cmd/git-sync ./internal/setup
git add cmd/git-sync internal/setup
git commit -m "feat(cli): install a mesh, uninstall every machine"
```

---

### Task 13: Repo check against every peer

**Files:**
- Modify: `internal/setup/repocheck.go`, `cmd/git-sync/stubs.go` (`checkPeer`)
- Test: `internal/setup/repocheck_test.go`

**Interfaces:**
- Consumes: `config.Peer` (Task 1), `PeerTarget` (Task 11).
- Produces: `func CheckPeers(peers []PeerTarget, repos []RepoWant, remotePrefs []string) map[string][]RepoCheck` keyed by peer host; `RenderRepoChecks(w io.Writer, host, peerBase string, checks []RepoCheck) int` unchanged per peer.

- [ ] **Step 1: Write the failing test**

```go
func TestCheckPeersAsksEveryMachine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"*": "present group/proj git@example.com:me/proj.git"}, 0)
	peers := []setup.PeerTarget{
		{Peer: config.Peer{Host: "b.local", User: "t"}, BaseDir: "/home/t/code"},
		{Peer: config.Peer{Host: "c.local", User: "t"}, BaseDir: "/home/t/code"},
	}
	want := []setup.RepoWant{{Rel: "group/proj", RemoteURL: "git@example.com:me/proj.git"}}

	got := setup.CheckPeers(peers, want, config.DefaultRemoteNames)
	if len(got) != 2 {
		t.Fatalf("got answers from %d machines, want 2: %+v", len(got), got)
	}
	for _, host := range []string{"b.local", "c.local"} {
		checks, ok := got[host]
		if !ok || len(checks) != 1 {
			t.Fatalf("no answer for %s: %+v", host, got)
		}
		if checks[0].State != setup.RepoPresent {
			t.Errorf("%s: state = %q, want present", host, checks[0].State)
		}
	}
}
```

Confirm the exact line format `repoCheckScript` emits before writing the stub reply above; copy it from the existing tests in `internal/setup/repocheck_test.go`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/setup/ -run CheckPeers -v`
Expected: FAIL — `undefined: setup.CheckPeers`.

- [ ] **Step 3: Implement**

```go
// CheckPeers asks every machine the same question, one round trip each. A
// repo that only exists on some machines is normal; what matters is that the
// user is told which machine is missing what, per machine.
func CheckPeers(peers []PeerTarget, repos []RepoWant, remotePrefs []string) map[string][]RepoCheck {
	out := make(map[string][]RepoCheck, len(peers))
	for _, pt := range peers {
		checks, err := CheckPeerReposWithRemotes(pt.Peer.Target(), pt.BaseDir, repos, remotePrefs)
		if err != nil {
			// An unreachable machine still gets a row per repo, marked
			// unchecked, so it renders as "we do not know" rather than
			// vanishing from the report.
			for i := range checks {
				checks[i].State = RepoUnchecked
			}
		}
		out[pt.Peer.Host] = checks
	}
	return out
}
```

In `cmd/git-sync/stubs.go`, `checkPeer` loops over the map and calls the existing `RenderRepoChecks` once per machine, summing the mismatch counts for the single confirm prompt that already exists.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/setup/ ./cmd/git-sync/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/setup ./cmd/git-sync
git add internal/setup cmd/git-sync
git commit -m "feat(setup): check selected repos on every machine"
```

---

### Task 14: Three-machine end-to-end tests

**Files:**
- Modify: `internal/syncer/e2e_test.go`
- Test: same file

**Interfaces:**
- Consumes: everything above.
- Produces: `machine(t, bin, name)` replacing `newPeerMachine`; `installLoopbackSSH(t, sb, machines...)` routing by host.

- [ ] **Step 1: Write the failing tests**

Replace the harness:

```go
// machine is one other machine in the mesh: its own $HOME, ~/.gitsync and
// base_dir, built by hand rather than through testutil.NewSandbox, which
// would collide with the machine already occupying this test process's
// environment. Only the loopback ssh stub ever runs anything against it.
type machine struct {
	Name    string // the hostname the stub routes on
	Home    string
	Gitsync string
	BaseDir string
}

func newMachine(t *testing.T, bin, name string) *machine {
	t.Helper()
	home := t.TempDir()
	m := &machine{
		Name:    name,
		Home:    home,
		Gitsync: filepath.Join(home, ".gitsync"),
		BaseDir: filepath.Join(home, "code"),
	}
	testutil.MkdirAll(t, filepath.Join(m.Gitsync, "bin"), filepath.Join(m.Gitsync, "locks"), m.BaseDir)
	copyExecutable(t, bin, filepath.Join(m.Gitsync, "bin", "git-sync"))
	return m
}

// saveConfig writes this machine's config.toml directly: config.Save would
// target the test process's own GITSYNC_HOME, which belongs to machine A.
func (m *machine) saveConfig(t *testing.T, repos []string, peers []config.Peer) {
	t.Helper()
	cfg := config.Config{BaseDir: m.BaseDir, Repos: repos, Peers: peers}
	b, err := cfg.MarshalFor(m.Name)
	if err != nil {
		t.Fatalf("marshal %s config: %v", m.Name, err)
	}
	if err := os.WriteFile(filepath.Join(m.Gitsync, "config.toml"), b, 0o644); err != nil {
		t.Fatalf("write %s config: %v", m.Name, err)
	}
}

// installLoopbackSSH puts a fake ssh on PATH that strips ssh's -o flags,
// routes on the user@host target, and runs the remaining command through a
// real shell with HOME and GITSYNC_HOME pointed at that machine. Every remote
// command git-sync issues - receive, the provisioning mkdir/cat/mv sequence,
// and a peer's own ssh to another peer during the key check - is just a shell
// command string, so one routing stub answers all of them.
func installLoopbackSSH(t *testing.T, sb *testutil.Sandbox, machines ...*machine) {
	t.Helper()
	var cases strings.Builder
	for _, m := range machines {
		fmt.Fprintf(&cases, `  *%s*) HOME=%s; GITSYNC_HOME=%s; GITCONFIG=%s ;;
`, m.Name, shellQuote(m.Home), shellQuote(m.Gitsync),
			shellQuote(filepath.Join(m.Home, ".gitconfig")))
	}
	script := fmt.Sprintf(`#!/bin/sh
while [ "$1" = "-o" ]; do
  shift 2
done
target="$1"
shift
case "$target" in
%s  *) echo "loopback ssh: unknown host $target" >&2; exit 255 ;;
esac
cmd="$1"
HOME="$HOME" GITSYNC_HOME="$GITSYNC_HOME" \
  GIT_CONFIG_GLOBAL="$GITCONFIG" GIT_CONFIG_SYSTEM=/dev/null \
  GIT_AUTHOR_NAME='git-sync test' GIT_AUTHOR_EMAIL='test@example.com' \
  GIT_COMMITTER_NAME='git-sync test' GIT_COMMITTER_EMAIL='test@example.com' \
  PATH="$PATH" \
  sh -c "$cmd"
`, cases.String())

	bin := filepath.Join(sb.Home, "bin")
	testutil.MkdirAll(t, bin)
	path := filepath.Join(bin, "ssh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write ssh stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
```

Keep `PATH` in the child environment so a peer's own `ssh` resolves back to this stub during the key check.

Then the new tests:

```go
func TestEndToEndCommitReachesEveryMachine(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")

	b := newMachine(t, bin, "b.local")
	c := newMachine(t, bin, "c.local")
	bRepo := b.clone(t, sb, "group/proj")
	cRepo := c.clone(t, sb, "group/proj")
	peers := []config.Peer{{Host: "b.local", User: "tester"}, {Host: "c.local", User: "tester"}}
	testutil.SaveConfigWithPeers(t, sb, peers, []string{"group/proj"})
	b.saveConfig(t, []string{"group/proj"}, append([]config.Peer{{Host: "a.local", User: "tester"}}, peers[1]))
	c.saveConfig(t, []string{"group/proj"}, append([]config.Peer{{Host: "a.local", User: "tester"}}, peers[0]))
	installLoopbackSSH(t, sb, b, c)

	testutil.Commit(t, sb, repo, "sync me")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	for name, dir := range map[string]string{"b.local": bRepo, "c.local": cRepo} {
		if out := sb.Git(dir, "log", "--oneline", "-1"); !strings.Contains(out, "sync me") {
			t.Errorf("commit did not reach %s:\n%s", name, out)
		}
	}
	assertHasEvent(t, b.events(t), activity.OpReceive, activity.StatusOK, "fast-forwarded")
	assertHasEvent(t, c.events(t), activity.OpReceive, activity.StatusOK, "fast-forwarded")
}

func TestEndToEndCommitIsRefusedWhileReceiving(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	other := sb.MakeRepo("group/other")
	testutil.SaveConfigWithPeers(t, sb,
		[]config.Peer{{Host: "b.local", User: "tester"}},
		[]string{"group/proj", "group/other"})

	// Install for real, so git runs the hooks we ship.
	if err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, Repos: []string{"group/proj", "group/other"},
		Self: bin, NoPeer: true, Out: io.Discard,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	l, err := lock.AcquireFrom("group/proj", "b.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()

	testutil.AppendFileIn(t, repo, "README.md", "edit\n")
	cmd := exec.Command("git", "commit", "-am", "should be refused")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("commit succeeded during a receive:\n%s", out)
	}
	if !strings.Contains(string(out), "b.local") {
		t.Errorf("git did not show git-sync's message:\n%s", out)
	}

	// A different repo is unaffected: the lock is per repo.
	testutil.AppendFileIn(t, other, "README.md", "edit\n")
	cmd = exec.Command("git", "commit", "-am", "allowed")
	cmd.Dir = other
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("an unrelated repo was blocked:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/syncer/ -run EndToEnd -v`
Expected: FAIL — `undefined: newMachine`, then a commit that is not refused.

- [ ] **Step 3: Make them pass**

No new production code should be needed. If the commit is not refused, check in this order: `core.hooksPath` is set (`git config --global core.hooksPath`), the `pre-commit` shim exists and is executable, and `Block` resolves the repo's relpath (`GITSYNC_HOME` must be the sandbox's). Fix whichever layer is actually wrong — `superpowers:systematic-debugging` if it is not obvious.

Port the remaining e2e tests (`TestEndToEndRepoThePeerDoesNotHave` and friends) to `newMachine`/`saveConfig`, keeping their assertions.

- [ ] **Step 4: Run the suite**

Run: `go test -race ./internal/syncer/ -v` then `make check`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w ./internal/syncer
git add internal/syncer
git commit -m "test(e2e): three-machine mesh and the receive block"
```

---

### Task 15: Docs and the checked-in binary

**Files:**
- Modify: `CLAUDE.md`, `README.md` (if present — check), `docs/superpowers/specs/2026-08-22-git-sync-design.md` (add a pointer to the new spec at the top)
- Modify: `git-sync` (rebuilt binary)

- [ ] **Step 1: Update CLAUDE.md**

- "between two machines" → "between two or more machines" in *What this is*.
- Replace the peer-pair description of `install` with the mesh: repeatable `--peer`, the pairwise key check, provisioning each machine with its own peer list.
- Architecture: add `pre-commit`/`pre-push` to the hook list and describe the receiver lock under `internal/lock` (owner record, `Held`, `Break`, the 60s heartbeat, `unlock`).
- Delete the `internal/secret`/`sshx` password paragraph and the `GITSYNC_SECRET_BACKEND` line under *Test sandboxing*; `sshx` is now "always BatchMode=yes, keys only".
- Test sandboxing: describe `machine(name)` and the routing loopback ssh stub instead of `peerMachine`.
- Key invariants: add "a machine that is receiving a repo refuses local commits and pushes in that repo, and never broadcasts a commit made during a receive" and "the hooks fail open: only a live lock blocks".
- Runtime layout: `hooks/{post-commit,pre-commit,pre-push}`, no `askpass`.

- [ ] **Step 2: Add a pointer at the top of the old spec**

```markdown
> **Superseded in part** by
> `2026-09-23-multi-machine-sync-design.md`, which replaces the two-machine
> peer pair with an N-machine mesh and removes SSH password support. The
> remote-as-transport model, the repo allowlist and the receive algorithm
> described here still hold.
```

- [ ] **Step 3: Verify the docs match the code**

```bash
grep -rn "askpass\|savepass\|peer_host\|GITSYNC_SECRET_BACKEND" CLAUDE.md README.md 2>/dev/null
```
Expected: no output except any deliberate "migrated from `peer_host`" mention.

- [ ] **Step 4: Rebuild and run the full check**

Run: `make check && make build`
Expected: PASS, and `./git-sync` rebuilt.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "docs: describe the mesh; rebuild the binary"
```

---

## Notes for the implementer

- **Read before you write.** Several tasks say "follow the existing style" — the existing tests in `internal/setup/` and `internal/syncer/` are the reference for fixtures and assertions, and the sketches here may differ in small details from what compiles.
- **`testutil.StubSSHScripted`'s reply patterns** are shell glob cases. Check its implementation before relying on a pattern shape; extend it rather than working around it.
- **The e2e suite is slow** (it compiles a binary per test). Run the package's fast tests while iterating and the e2e ones before committing.
- **Task 2 deletes a lot.** Let the compiler drive it: delete the package, then fix every error it reports, rather than grepping first.
