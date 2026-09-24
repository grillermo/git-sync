# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`git-sync` is a single Go binary that keeps a chosen set of git repos in sync
between two or more machines. `git-sync install <base_dir>` (run once, on one
machine, with a repeatable `--peer user@host[:base_dir]` for every other
machine in the mesh) scans for repos, opens a checkbox picker, wires up a
global git hook, checks that every pair of machines in the mesh can ssh to
each other (not just this machine to each peer — a missing key between two
*peers*, a pair this machine never exercises itself, would otherwise only
surface later as a failing notify nobody is watching), and provisions each
machine over SSH with its own peer list — nothing is typed on any machine but
the one running `install`. After that, every commit in a selected repo
pushes and notifies every other machine in the mesh in parallel, in the
background. `git-sync report` shows a TUI of sync activity; `git-sync
uninstall` removes it from every machine in the mesh.

**The shared git remote is the transport, not SSH.** A commit is pushed to
the repo's own remote; the peer pulls it back down from that same remote.
SSH carries exactly one thing: the notification that there is something to
pull. The remote is resolved by name per repo — `github` if it exists, else
`origin`, else the repo's sole remote — never from the branch's
`@{upstream}`, because a branch can track `origin/main` while the repo's real
shared remote is `github`, and both machines must agree on which one they
meet at. See `docs/superpowers/specs/2026-08-22-git-sync-design.md` for the
original two-machine design spec (including verified environment facts about
the pinned dependency versions) and
`docs/superpowers/specs/2026-09-23-multi-machine-sync-design.md` for the
mesh design that supersedes its peer-pair and SSH-password parts; the
task-by-task implementation plans are the correspondingly-named files under
`docs/superpowers/plans/`.

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
`testutil.NewSandbox(t)`, which points `HOME`, `GITSYNC_HOME`, and
`GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` at an isolated temp tree via
`t.Setenv`. This is why `NewSandbox`-based tests can never run with
`t.Parallel()` — they share this process's environment and shell out to a
real `git` binary against it. Never write a test that touches `~/.gitsync`
or `~/.gitconfig` directly.

`testutil.Sandbox` has fixtures for most scenarios already: `MakeRepo`,
`MakeRepoNamedRemote`, `AddRemote`, `PeerClone`/`PeerCommit`, `Dirty`,
`StubSSH`/`StubSSHFailing`/`StubSSHScripted`, and
`SaveConfig`/`SaveConfigWithRepos`/`SaveConfigWithPeers`/`SaveConfigWithRemotes`.
Check there before writing a new one-off fixture.

Two env vars exist solely as test escape hatches, both defaulting to the
spec's real values: `GITSYNC_HOME` (default `~/.gitsync`) sandboxes an
entire fake machine, and `GITSYNC_LOCK_TIMEOUT` (default 30s, see
`internal/lock`) speeds up lock-contention tests. (`GITSYNC_INTERNAL=1` is
also set, but by `gitcmd.Run` on every git command git-sync issues, not by
tests — it is how `pre-commit`/`pre-push` tell git-sync's own pushes and
merges apart from a real user commit, see Architecture below.)

`internal/syncer/e2e_test.go` goes one step further, for a real multi-machine
mesh: it builds a real binary per test (`buildBinary`), hand-builds each
*other* machine's `~/.gitsync` by hand (`newMachine(t, bin, name)` returns a
`*machine` with its own `Home`/`Gitsync`/`BaseDir` — a real `testutil.Sandbox`
already occupies this process's env, so a second one can't coexist in the
same process; the sandboxed machine and any number of `*machine`s stand in
for the rest of the mesh), and installs one generic loopback `ssh` stub
(`installLoopbackSSH(t, sb, machines...)`) that strips ssh's `-o` flags,
routes on the `user@host` target to whichever machine's `HOME`/`GITSYNC_HOME`
it names, and re-runs whatever remote command is left through a real shell
pointed at that machine. Because every ssh invocation git-sync makes —
receive, the provisioning mkdir/cat/mv/git-config sequence, and a peer's own
ssh to another peer during the pairwise key check — is just a shell command
string, this one routing stub answers all of them with no per-command
special-casing, for any number of machines in the mesh.

## Architecture

Seven subcommands off one binary (`cmd/git-sync/main.go` dispatches; the
actual command bodies live in `cmd/git-sync/stubs.go` — despite the
filename, that file is not stub code, it's the real implementation of every
`cmdX` function). Four are for humans (`install`, `uninstall`, `report`,
`unlock` — clears a stuck receiver lock by hand); three are invoked by
machines and deliberately hidden from `-h` output (`hook`, `push`,
`receive`). Key-only ssh auth means nothing else needs to shell out to
git-sync itself, so there is no `askpass`/`savepass` anymore.

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
  (`StaleAfter`, 5 minutes), so rapid consecutive commits on the pusher don't
  race their receives' stash/fetch/merge steps against each other, and so
  `pre-commit`/`pre-push` can tell a commit is unsafe right now. `Acquire`/
  `AcquireFrom` write an `Owner` record (`from`, `started`, `pid`) into the
  lock directory — not just a bare mkdir — so a blocked hook can name which
  machine is mid-sync rather than just refusing; `Held(rel)` reads that
  record back (a lock older than `StaleAfter` reads as not held, since its
  holder is assumed dead) and is what the blocking hooks call. `Break(rel)`
  is `unlock`'s primitive: removes the lock by hand and reports who held it.
  A receive that outlives `StaleAfter` (a large fetch) restamps its own lock
  every minute via `Refresh` so it never goes stale out from under itself —
  see `heartbeat` in `receive.go`.
- **`internal/syncer`** — the machine-invoked operations, composed from the
  above:
  - `hook.go`: `Hook` runs on every `post-commit` in every repo on the
    machine (via global `core.hooksPath`), but only *acts* on repos in the
    allowlist. Must do almost nothing itself — git waits on this process —
    so it identifies the repo and re-execs itself detached as `push`.
    `Block` runs on `pre-commit` and `pre-push`: if the repo is selected and
    currently locked by a receive, it refuses (exit 1) with a message naming
    which machine is mid-sync and how to `git-sync unlock` it; otherwise it
    is a no-op. It also lets git-sync's own git commands through unconditionally
    via `GITSYNC_INTERNAL=1` (set by `gitcmd.Run` on every command git-sync
    itself issues, e.g. the background push, `initialsync`'s merges), so
    git-sync never blocks on its own lock.
  - `push.go`: pushes the current branch to the resolved remote, then SSHes
    every peer in the mesh in parallel to run its own `receive --from
    <this-machine>`. No retry queue by design — a failed push or unreachable
    peer just gets carried by the next commit.
  - `receive.go`: `Receive(rel, from)` acquires the lock (recording `from`,
    the notifying machine, in the `Owner`), fetches, stashes if dirty,
    fast-forwards, unstashes — unconditionally, whether or not the
    fast-forward succeeded, so a stashed tree is never silently lost. Never
    commits or pushes itself, so there's no feedback loop back to the peer's
    hook, and — because the lock is held for the whole operation — any local
    commit attempted mid-receive is refused by `pre-commit`/`pre-push` and
    so can never itself get broadcast out.
- **`internal/scan`** / **`internal/picker`** — repo discovery under
  `base_dir`, and a thin adapter (`Rows`/`Config`/`Choose`) that hands the
  scan to `github.com/grillermo/chicle` (pinned at a published tag, not a
  `replace`) as a multi-select list for choosing which to sync. The UI itself
  lives in chicle and draws on `/dev/tty`.
- **`internal/setup`** — `install.go` (`Install`/`Uninstall` for the local
  machine, plus mesh-wide `UninstallMesh` which sshes each peer to run its
  own local uninstall before cleaning up here — copies the binary, writes the
  `post-commit`/`pre-commit`/`pre-push` hook shims, sets `core.hooksPath`),
  `provision.go` (pushes binary/config/hook to one peer over ssh, given that
  peer's own `Options.Peers` list, idempotently — called once per machine in
  the mesh), `repocheck.go` (asks each reachable peer which selected repos it
  actually has, and whether they point at the same remote — a mismatched
  pair silently never converges otherwise), `keycheck.go` (`CheckKeys` probes
  every *ordered pair* of machines in the mesh, not just this machine to each
  peer — a peer-to-peer check runs by sshing onto `from` and having it in
  turn `ssh ... to true`; `RenderKeyChecks` prints the pairs that fail and
  how to fix them with `ssh-copy-id`, and install carries on regardless,
  since the rest of the mesh is still worth setting up), `sshauth.go`
  (`Reachable` — a key-only connectivity probe; there is no password prompt
  path anymore), `initialsync.go` (the last install stage: measures every
  machine in the mesh against the shared remote, then pushes whichever side
  is purely ahead and fast-forwards whichever is purely behind).

  `initialsync.go` exists because `receive` only ever fast-forwards. One
  unpushed commit sitting on any machine at install time makes every later
  sync warn instead of applying, forever, and nothing retries it — `receive`
  never pushes, so the divergence cannot resolve itself. It uses only push
  and `merge --ff-only`, stashing around the merge exactly as `receive` does,
  so it can never add anything to history a normal sync would not. Genuinely
  diverged history, and machines on different branches, are reported for the
  user to merge by hand — never merged automatically. `--no-initial-sync`
  skips the stage.
- **`internal/report`** — `aggregate.go` is pure functions over a slice of
  `activity.Event` (no I/O, no terminal — trivially testable), `plain.go` is
  static output for piped/non-tty use, `tui.go` is the bubbletea browser.

Runtime layout under `~/.gitsync/` (or `$GITSYNC_HOME`): `bin/git-sync` (the
copy the hooks and ssh invoke — re-copying on install can't race a commit
mid-execution), `hooks/{post-commit,pre-commit,pre-push}` (shell shims, not
the binary itself, for the same non-racing reason — each just execs `git-sync
hook <name>`), `config.toml`, `activity.jsonl`, `debug.log`, `locks/`. No
`askpass`: ssh is key-only now, so there is nothing for one to feed.

## Key invariants worth preserving

- `core.hooksPath` is **global and exclusive** — it replaces, not chains
  with, any repo-local hooks (Husky, the `pre-commit` framework, etc.) —
  including the *name* `pre-commit`: git-sync's own hook of that name is a
  different thing from the popular `pre-commit` tool's hook, and installing
  git-sync means that tool's hook no longer runs on its own.
- A repo not in the config's `Repos` allowlist is a silent no-op everywhere
  (hook, push, receive) — sync is opt-in per repo, not per machine.
- A one-sided repo (exists on only one machine) or a repo cloned from a
  *different* remote than the peer's copy must never look like success: it's
  a `skip`/`warn` event, never silently dropped and never retried
  automatically.
- Nothing in the sync path may ever block on a terminal prompt — the hook
  and its children run detached with no tty. This is why ssh is
  `BatchMode=yes`, key-only, everywhere (`internal/sshx`) rather than ever
  falling back to an interactive password.
- A machine that is receiving a repo refuses local commits and pushes in
  that repo, and never broadcasts a commit made during a receive —
  `pre-commit`/`pre-push` check the same lock `receive` holds, and `push.go`
  is never reached because the commit itself is refused first.
- The hooks fail open: only a *live* lock blocks. A crashed receive's lock
  goes stale after `StaleAfter` and stops blocking on its own, and
  `git-sync unlock` clears one by hand — a wedged lock must never be able to
  permanently stop a repo from taking commits.

## Dependency pins (Go 1.26)

`bubbletea v1.3.10` + `bubbles v1.0.0` + `lipgloss v1.1.0` — pin the **v1**
API surface (`Init() tea.Cmd`, `Update(tea.Msg) (tea.Model, tea.Cmd)`,
`viewport.New(width, height)`). A `v2` exists upstream as a prerelease with a
different signature; published docs mix the two freely, so don't "correct"
existing code to a signature seen elsewhere without checking which major
version it's from. `BurntSushi/toml v1.6.0`, `golang.org/x/term`. No
external test runner — plain `go test`.
