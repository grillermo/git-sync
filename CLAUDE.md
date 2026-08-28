# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`git-sync` is a single Go binary that keeps a chosen set of git repos in sync
between two machines. `git-sync install <base_dir>` (run once, on one
machine) scans for repos, opens a checkbox picker, wires up a global git
hook, and provisions the peer over SSH — nothing is typed on the second
machine. After that, every commit in a selected repo pushes and notifies the
peer in the background. `git-sync report` shows a TUI of sync activity;
`git-sync uninstall` removes it cleanly.

**The shared git remote is the transport, not SSH.** A commit is pushed to
the repo's own remote; the peer pulls it back down from that same remote.
SSH carries exactly one thing: the notification that there is something to
pull. The remote is resolved by name per repo — `github` if it exists, else
`origin`, else the repo's sole remote — never from the branch's
`@{upstream}`, because a branch can track `origin/main` while the repo's real
shared remote is `github`, and both machines must agree on which one they
meet at. See `docs/superpowers/specs/2026-08-22-git-sync-design.md` for the
full design spec and `docs/superpowers/plans/2026-08-22-git-sync.md` for the
task-by-task implementation plan (including verified environment facts about
the pinned dependency versions).

## Commands

```bash
make build          # go build -o git-sync ./cmd/git-sync
make test           # go test ./...
make lint           # go vet ./... && gofmt -l .
make check          # lint + test — run this before considering anything done
make install BASE_DIR=~/code   # build then run: ./git-sync install $(BASE_DIR)
```

After successfully adding a new feature (checks green), always rebuild the
binary with `make build` so the checked-in `./git-sync` reflects it.

Single test / package:

```bash
go test ./internal/syncer/...
go test -run TestPushUsesTheGithubRemoteWhenThereIsOne ./internal/syncer/
go test -race ./...              # required for internal/activity — concurrent appends
go test -race -run TestEndToEnd ./internal/syncer/...   # the e2e suite; slow, compiles a real binary
```

`gofmt -w ./internal/<pkg>` before committing — `make lint` fails on
unformatted files, not just vet issues.

## Test sandboxing

Every test that touches the filesystem or git goes through
`testutil.NewSandbox(t)`, which points `HOME`, `GITSYNC_HOME`,
`GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM`, and `GITSYNC_SECRET_BACKEND=file` at
an isolated temp tree via `t.Setenv`. This is why `NewSandbox`-based tests
can never run with `t.Parallel()` — they share this process's environment
and shell out to a real `git` binary against it. Never write a test that
touches `~/.gitsync`, `~/.gitconfig`, or the real OS keychain directly.

`testutil.Sandbox` has fixtures for most scenarios already: `MakeRepo`,
`MakeRepoNamedRemote`, `AddRemote`, `PeerClone`/`PeerCommit`, `Dirty`,
`StubSSH`/`StubSSHFailing`/`StubSSHPassword`/`StubSSHScripted`, and
`SaveConfig`/`SaveConfigWithRepos`/`SaveConfigWithRemotes`. Check there
before writing a new one-off fixture.

Three env vars exist solely as test escape hatches, all defaulting to the
spec's real values: `GITSYNC_HOME` (default `~/.gitsync`) sandboxes an
entire fake machine, `GITSYNC_LOCK_TIMEOUT` (default 30s, see
`internal/lock`) speeds up lock-contention tests, and
`GITSYNC_SECRET_BACKEND` (default: OS keychain; `file`/`blackhole` for
tests) keeps tests off real OS keychain state — important on macOS, where
reading the real keychain can raise a GUI prompt mid test run.

`internal/syncer/e2e_test.go` goes one step further: it builds a real binary
per test (`buildBinary`), hand-builds a *second* machine's `~/.gitsync`
(`peerMachine`, since a real `testutil.Sandbox` occupies this process's env
and a second one can't coexist in the same process), and installs a generic
loopback `ssh` stub (`installLoopbackSSH`) that strips ssh's `-o` flags and
the `user@host` target and re-runs whatever remote command is left through a
real shell with `HOME`/`GITSYNC_HOME` pointed at the peer. Because every ssh
invocation git-sync makes — receive, and the whole provisioning sequence of
mkdir/cat/mv/git-config — is just a shell command string, this one stub
answers all of them with no per-command special-casing.

## Architecture

Six subcommands off one binary (`cmd/git-sync/main.go` dispatches; the
actual command bodies live in `cmd/git-sync/stubs.go` — despite the
filename, that file is not stub code, it's the real implementation of every
`cmdX` function). Three are for humans (`install`, `uninstall`, `report`);
three are invoked by machines and deliberately hidden from `-h` output
(`hook`, `push`, `receive`), plus `askpass`/`savepass` invoked by ssh itself.

Package layering, leaves to composition:

- **`internal/config`** — `Config` struct, `Load`/`Save`, and the
  `base_dir`-relative repo identity rules (`RepoRel`/`RepoPath`/`IsSelected`/
  `ValidateRel`). `ValidateRel` matters because repo relpaths arrive
  untrusted over ssh from the peer. `config.Home()` (via `GITSYNC_HOME` or
  `~/.gitsync`) is the one function everything else's path helpers key off.
- **`internal/activity`** — the structured, append-only JSON-lines event log
  (`activity.jsonl`) that `report` reads and every other package writes to.
  Lines are kept under `MaxLineLen` (POSIX `PIPE_BUF`) so that concurrent
  `O_APPEND` writes from separate push/receive processes never interleave —
  this is *why* there's no lock around the log itself, and why
  `go test -race` on this package matters.
- **`internal/gitcmd`** — thin exec wrapper over the real `git` binary
  (never a git library, so it behaves exactly like the user's own git,
  credentials and hooks included). Owns `ResolveRemote`, the shared logic
  behind the `github` → `origin` → sole-remote preference rule that push and
  receive both call so they can never disagree about where "the remote" is.
- **`internal/lock`** — per-repo `mkdir`-based lock with stale reclaim
  (`StaleAfter`), so rapid consecutive commits on the pusher don't race their
  receives' stash/fetch/merge steps against each other.
- **`internal/syncer`** — the three machine-invoked operations, composed from
  the above:
  - `hook.go`: runs on every commit in every repo on the machine (via global
    `core.hooksPath`), but only *acts* on repos in the allowlist. Must do
    almost nothing itself — git waits on this process — so it identifies the
    repo and re-execs itself detached as `push`.
  - `push.go`: pushes the remote's **default branch** (`gitcmd.DefaultBranch`,
    resolved fresh every call), not whatever is checked out, to the resolved
    remote, then SSHes the peer to run its own `receive`. A commit on any
    other branch leaves the default branch unmoved, so pushing it is a
    harmless no-op — that's how "non-default branches don't sync" falls out
    for free instead of needing a guard. No local default branch is a skip,
    never auto-created. No retry queue by design — a failed push or
    unreachable peer just gets carried by the next commit.
  - `receive.go`: locks, fetches, and fast-forwards the local **default
    branch** — the same one push targets, regardless of what's checked out
    here. If the default branch is checked out, it's the familiar
    stash-if-dirty / `merge --ff-only` / unstash-unconditionally dance; if
    it's not checked out, `gitcmd.FastForwardRef` advances the ref directly
    with no stash needed, since there's no worktree to disturb. Diverged
    history is always `StatusWarn`, fetched-only, manual-merge-needed —
    receive itself never auto-merges, in either case. Never commits or
    pushes itself, so there's no feedback loop back to the peer's hook.
- **`internal/scan`** / **`internal/picker`** — repo discovery under
  `base_dir` and the bubbletea checkbox TUI for choosing which to sync.
- **`internal/setup`** — `install.go` (local install/uninstall — copies the
  binary, writes the hook shim, sets `core.hooksPath`), `provision.go` (pushes
  binary/config/hook to the peer over ssh, idempotently), `repocheck.go` (asks
  the peer which selected repos it actually has, and whether they point at
  the same remote — a mismatched pair silently never converges otherwise),
  `sshauth.go` (detects a password-only peer, prompts once, verifies against
  the peer, stores in the keychain before anything else happens),
  `initialsync.go` (the last install stage: measures both machines' **default
  branch**, resolved the same way push/receive do, against the shared remote,
  then pushes whichever side is purely ahead and fast-forwards whichever is
  purely behind).

  `initialsync.go` exists because `receive` only ever fast-forwards. One
  unpushed commit sitting on either machine at install time makes every
  later sync warn instead of applying, forever, and nothing retries it —
  `receive` never pushes, so the divergence cannot resolve itself. Passes 1–3
  use only push and `merge --ff-only`, stashing around the merge exactly as
  `receive` does, so they can never add anything to history a normal sync
  would not. On top of that, install (and only install) will attempt **one
  real merge** on a genuinely diverged repo: `landMerge` merges
  `<remote>/<branch>` into the diverged side, only when the default branch is
  the checked-out HEAD there (a not-checked-out diverged branch has no
  worktree to merge into, so it's reported blocked instead); a clean merge is
  pushed and the other side then fast-forwards onto it in pass 3, a
  conflicting merge is `git merge --abort`ed and the repo is reported for the
  user to merge by hand, same as before. `syncApplyScript` mirrors this
  exact logic in shell for the case where the *peer* is the diverged side. No
  local default branch (only feature branches checked out, nothing named
  `main`/`master`/`trunk` locally) is reported and skipped, never
  auto-created. Two machines resolving to different branch names is kept as a
  defensive guard but should no longer fire in practice, since both sides now
  resolve the same remote's default branch. `--no-initial-sync` skips the
  whole stage.
- **`internal/secret`** / **`internal/sshx`** — the peer's ssh password (OS
  keychain via `security`/`libsecret`, or `file`/`blackhole` backends for
  tests) and the ssh command builder that wires `SSH_ASKPASS` in only when a
  password is actually stored (`BatchMode=yes` otherwise, since nothing can
  answer a prompt from a detached hook).
- **`internal/report`** — `aggregate.go` is pure functions over a slice of
  `activity.Event` (no I/O, no terminal — trivially testable), `plain.go` is
  static output for piped/non-tty use, `tui.go` is the bubbletea browser.

Runtime layout under `~/.gitsync/` (or `$GITSYNC_HOME`): `bin/git-sync` (the
copy the hook and ssh invoke — re-copying on install can't race a commit
mid-execution), `hooks/post-commit` (a shell shim, not the binary itself,
for the same non-racing reason), `askpass`, `config.toml`, `activity.jsonl`,
`debug.log`, `locks/`.

## Key invariants worth preserving

- `core.hooksPath` is **global and exclusive** — it replaces, not chains
  with, any repo-local hooks (Husky, `pre-commit`, etc.).
- A repo not in the config's `Repos` allowlist is a silent no-op everywhere
  (hook, push, receive) — sync is opt-in per repo, not per machine.
- A one-sided repo (exists on only one machine) or a repo cloned from a
  *different* remote than the peer's copy must never look like success: it's
  a `skip`/`warn` event, never silently dropped and never retried
  automatically.
- Nothing in the sync path may ever block on a terminal prompt — the hook
  and its children run detached with no tty. This is the entire reason the
  password/keychain machinery in `setup`/`secret`/`sshx` exists.
- **Every sync operation targets the remote's default branch**
  (`gitcmd.DefaultBranch`, resolved per repo — `main`, `master`, `trunk`,
  whatever the remote's `HEAD` says), never whatever happens to be checked
  out locally. A commit on any other branch leaves the default branch
  unmoved, so it simply doesn't sync — there is no separate guard for "only
  the default branch syncs," it falls out of this rule for free.
- **Auto-merge is install-time-only, and only on a clean merge.** Steady-state
  `receive` stays fast-forward-only forever — diverged history there is
  always reported and left for the user, never merged, because two machines
  each auto-merging would mint rival merge commits that re-diverge forever
  and `receive` never pushes so it couldn't fix that itself. The one
  exception: `install`'s initial sync may resolve a genuinely diverged
  default branch with a single real merge, but only on the diverged side,
  only once, only when the default branch is checked out there, and only if
  the merge is clean — a conflict is always `git merge --abort`ed and
  reported for the user exactly as an unresolved divergence always has been.

## Dependency pins (Go 1.26)

`bubbletea v1.3.10` + `bubbles v1.0.0` + `lipgloss v1.1.0` — pin the **v1**
API surface (`Init() tea.Cmd`, `Update(tea.Msg) (tea.Model, tea.Cmd)`,
`viewport.New(width, height)`). A `v2` exists upstream as a prerelease with a
different signature; published docs mix the two freely, so don't "correct"
existing code to a signature seen elsewhere without checking which major
version it's from. `BurntSushi/toml v1.6.0`, `golang.org/x/term`. No
external test runner — plain `go test`.
