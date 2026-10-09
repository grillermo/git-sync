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
make build          # ./build: bin/git-sync-<os>-<arch> for all four targets; bin/git-sync -> this machine's
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

Twelve subcommands off one binary (`cmd/git-sync/main.go` dispatches; the
actual command bodies live in `cmd/git-sync/stubs.go` — despite the
filename, that file is not stub code, it's the real implementation of every
`cmdX` function). Six are for humans (`install`, `uninstall`, `report`,
`unlock` — clears a stuck receiver lock by hand, `service` — installs or
removes the login service mesh-wide, or with `--local` just here, `activate
[<repo>]` — runs a repo's `./activate` now); six are
invoked by machines and deliberately hidden from `-h` output (`hook`, `push`,
`receive`, `retry`, `announce`, `watch`; `activate --drain` is the hidden
machine form of `activate`). Key-only ssh auth means nothing else needs to shell out to
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
    <this-machine>`. A rejected push is not retried (it needs a human, and
    the next commit in that repo tries again).
  - `pending.go`: the offline-resilience queue. A delivery that failed only
    because a machine was out of reach — a push `gitcmd.IsOffline` reads as
    unreachable, a notify where ssh exits 255, or a peer whose receive could
    not fetch (`ExitFetchFailed`, 4) — is recorded as an empty marker file
    (`pending/push/<rel>`, `pending/notify/<host>/<rel>`, rel path-escaped;
    atomic create/remove, so no lock). Every later `push` of *any* repo
    retries the backlog afterwards (skipping a peer it just found down), and
    `receive` spawns a detached `retry` when the backlog is non-empty, since
    being notified proves this machine is back online. Entries for repos no
    longer selected are dropped. `sshx` adds `ServerAliveInterval`/`CountMax`
    so a peer that vanishes mid-receive turns into exit 255 after ~1 minute
    instead of hanging the background push.
  - `watch.go`: the long-running process the login service runs. Announces
    once at start (login) and again on every wake from sleep. No user-level,
    cgo-free sleep notification exists on either OS (IOKit on macOS, root's
    system-sleep hooks on Linux), so a wake is read off the clocks: it
    sleeps `Interval` on the monotonic clock, which stops during system
    sleep, and a *wall-clock* gap beyond `Interval+Slack` means the machine
    slept. Exits `ExitRestart` (75) when the installed binary is replaced, so
    the service's restart-on-failure brings it back on the new version, and
    0 once the binary or the unit is gone (uninstalled — stays stopped).
    SIGTERM is honoured only at safe points: `announce` checks its stop
    channel between repos and rounds, never inside a receive. Every clock,
    sleep and announce is injectable via `WatchOptions` for tests.
  - `announce.go`: what `watch` runs at login and on wake (also callable
    directly as `git-sync announce`).
    Retries with backoff for up to 5 minutes (`GITSYNC_ANNOUNCE_TIMEOUT`, a
    test escape hatch) because a login service usually starts before the
    network: catches every selected repo up from the remote by the same
    path as `receive` (so a commit whose author machine is itself off now is
    still picked up), sshes every peer to run its `retry` (so it sends what
    it owes this machine), and flushes this machine's own backlog. A peer
    still unreachable at the deadline is only noted in `debug.log`: it is
    presumably off and will announce itself.
  - `receive.go`: `Receive(rel, from)` acquires the lock (recording `from`,
    the notifying machine, in the `Owner`), fetches, stashes if dirty,
    fast-forwards, unstashes — unconditionally, whether or not the
    fast-forward succeeded, so a stashed tree is never silently lost. Never
    commits or pushes itself, so there's no feedback loop back to the peer's
    hook, and — because the lock is held for the whole operation — any local
    commit attempted mid-receive is refused by `pre-commit`/`pre-push` and
    so can never itself get broadcast out.
  - `activate.go`: runs a repo's executable `./activate` after a sync lands
    new code on this machine (build, restart a service). `receive` only
    *enqueues*, after a fast-forward that moved HEAD in a repo that has one,
    into `~/.gitsync/activate/queue/` — one file per repo, holding the
    pre-sync rev, created with `link(2)` so it is create-if-absent: a repo is
    queued at most once and keeps the *oldest* rev, so three quick commits
    mean one run. The `syncer` package never starts a process itself; the
    command layer (`kickActivate` in `stubs.go`) spawns a detached `git-sync
    activate --drain` after `receive` and `announce`, and `watch` does the
    same via `WatchOptions.AfterAnnounce` (which also picks up anything a
    crash left queued). The drainer takes one machine-wide lock
    (`locks/.activate.lock`), so runs are serial per machine, and if the lock is
    held it just exits — the holder will see the new entry. Each run pops the
    entry *before* running (a sync landing mid-run re-queues the repo) and
    holds the repo's own receive lock with `Owner.From == "./activate"`, so
    no fast-forward rewrites the tree under a build and `pre-commit`/
    `pre-push` refuse commits, naming the running `./activate`. A busy repo
    postpones its entry (re-queued, round stops; that receive's own drainer
    resumes). The script runs in the repo root in its own process group with
    `GITSYNC_OLD_REV`/`GITSYNC_NEW_REV` set and `GITSYNC_INTERNAL` stripped
    (so a commit it tries is refused like any other); output goes to
    `~/.gitsync/activate/<rel>.log` (rotated at 256 KiB). A timeout kills the
    whole group; `GITSYNC_ACTIVATE_TIMEOUT` (default 15m) is a test escape
    hatch. Success, failure and timeout are `activate` events in the
    activity log. `git-sync activate [<repo>]` is the manual form: it waits
    for the drain lock, runs in the foreground with output teed to the
    terminal, drops that repo's queue entry, and passes git's empty-tree hash
    as `GITSYNC_OLD_REV` so `git diff OLD NEW` sees every file as changed.
    Ctrl-C/SIGTERM kills the script's group and releases both locks (a second
    Ctrl-C kills git-sync outright). Entries for repos no longer selected or
    no longer having a `./activate` are dropped with a `skip` event.
    `initialsync` does not enqueue.
- **`internal/scan`** / **`internal/picker`** — repo discovery under
  `base_dir`, and a thin adapter (`Rows`/`Config`/`Choose`) that hands the
  scan to `github.com/grillermo/chicle` (pinned at a published tag, not a
  `replace`) as a multi-select list for choosing which to sync. The UI itself
  lives in chicle and draws on `/dev/tty`.
- **`internal/discovery`** — local-network peer discovery for `install`,
  ported from vvterm's `LocalSSHDiscoveryService`: a Bonjour browse for
  `_ssh._tcp`/`_sftp-ssh._tcp` (hand-rolled mDNS via
  `golang.org/x/net/dns/dnsmessage`, sent from an ephemeral port as a
  "legacy unicast" query so it never needs to bind 5353) alongside a
  port-22 sweep of this machine's /24, for `ScanDuration`. `Set` merges the
  two — a port-scan IP that a Bonjour host owns folds into that host — and
  `Scan` drops this machine itself. `picker.ChoosePeers` shows the results
  in chicle as they arrive (via `chicle.Config.Updates`). Discovery only
  suggests: a found host becomes a peer only after the user ticks it, and
  still goes through `Peer.Validate` (a hostname off the network ends up in
  remote shell commands) and the key-only reachability check. Runs on a
  terminal when there is no peer yet, or always with `--discover`. Every
  found host is offered by IPv4, never its `.local` name (a Bonjour hit
  waits for its A record; a swept address beats an advertised one): ssh
  keys known_hosts on the exact name typed, so a machine trusted by IP still
  fails by name. Likewise `setup.SelfHost` defaults the address peers reach
  this machine back on to its LAN IP, not `os.Hostname()`.
- **`internal/setup`** — `install.go` (`Install`/`Uninstall` for the local
  machine, plus mesh-wide `UninstallMesh` which sshes each peer to run its
  own local uninstall before cleaning up here — copies the binary, writes the
  `post-commit`/`pre-commit`/`pre-push` hook shims, sets `core.hooksPath`),
  `provision.go` (pushes binary/config/hook to one peer over ssh, given that
  peer's own `Options.Peers` list, idempotently — called once per machine in
  the mesh; `binaryFor` sends this machine's binary to a same-platform peer,
  else the `git-sync-<os>-<arch>` build from `PeerOptions.Builds`, the
  directory `./build` filled next to the binary `install` was run from — it
  never cross-compiles, and a missing build is an error naming the `./build`
  target to run), `repocheck.go` (asks each reachable peer which selected repos it
  actually has, and whether they point at the same remote — a mismatched
  pair silently never converges otherwise), `clone.go` (`ClonePeerRepos`:
  a repo that check finds *missing* on a peer is cloned there after the
  install, from this machine's remote URL under this machine's remote name
  and branch, before levelling — never over an existing path, never with a
  prompt; `--no-clone` skips it), `keycheck.go` (`CheckKeys` probes
  every *ordered pair* of machines in the mesh, not just this machine to each
  peer — a peer-to-peer check runs by sshing onto `from` and having it in
  turn `ssh ... to true`; `RenderKeyChecks` prints the pairs that fail with
  their fixes, and install carries on regardless, since the rest of the mesh
  is still worth setting up), `sshfix.go` (`FixesFor` reads ssh's output to
  tell an unknown host key from a changed one, a rejected key, sshd off, or an
  unresolvable name, and builds the exact interactive commands for that pair
  — wrapped in `ssh -t <from>` when the pair runs on a peer; install offers to
  run them, with a changed host key confirmed on its own), `sshauth.go`
  (`Reachable` — a key-only connectivity probe; there is no password prompt
  path anymore), `initialsync.go` (the last install stage: measures every
  machine in the mesh against the shared remote, then pushes whichever side
  is purely ahead and fast-forwards whichever is purely behind).

  `service.go` writes the login service that runs `watch`: a
  LaunchAgent plist in `~/Library/LaunchAgents` on macOS, a systemd user unit
  (enabled by writing the `default.target.wants` symlink by hand) under
  `$XDG_CONFIG_HOME/systemd/user` on Linux. The files alone
  make it start at every login (launchd restarts it only on failure —
  `KeepAlive`/`SuccessfulExit=false`; systemd `Restart=on-failure`). Starting
  it right now goes through `launchctl bootout`+`bootstrap` / `systemctl
  --user daemon-reload`+`restart`, best effort only: over ssh there may be no
  GUI session or user bus, and a failure just means it starts at the next
  login. `NewSandbox` stubs `launchctl`/`systemctl` on PATH (calls logged,
  see `ServiceCalls`) and sandboxes `XDG_CONFIG_HOME`, so no test can load a
  unit into the real session. `install` sets it up
  on every machine (`--no-service` skips; a peer's unit is written by the
  peer's own binary via `service install --local`), `uninstall` removes it.
  `ServiceMesh` is the standalone `git-sync service install` for an existing
  mesh: it also upgrades each machine's binary (via `sendBinary`, shared
  with `ProvisionPeer`), since older ones have no `announce`.

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

`cmd/git-sync/pending.go` saves the picker's repo selection in the temp
dir until a run pairs every machine, so repeated installs while fixing ssh
reopen the picker with it pre-ticked (and reuse it as is without a terminal).
`testutil.NewSandbox` points `TMPDIR` into the sandbox for that reason.

Runtime layout under `~/.gitsync/` (or `$GITSYNC_HOME`): `bin/git-sync` (the
copy the hooks and ssh invoke — re-copying on install can't race a commit
mid-execution), `hooks/{post-commit,pre-commit,pre-push}` (shell shims, not
the binary itself, for the same non-racing reason — each just execs `git-sync
hook <name>`), `config.toml`, `activity.jsonl`, `debug.log`, `locks/`, `pending/`, `activate/` (`queue/` plus per-repo `*.log`). No
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
- Only receiving machines run `./activate`, never the committing one, and one
  at a time per machine. A failed or timed-out run is an event in the
  activity log, never retried automatically (the next sync that moves the
  repo queues it again).

## Dependency pins (Go 1.26)

`bubbletea v1.3.10` + `bubbles v1.0.0` + `lipgloss v1.1.0` — pin the **v1**
API surface (`Init() tea.Cmd`, `Update(tea.Msg) (tea.Model, tea.Cmd)`,
`viewport.New(width, height)`). A `v2` exists upstream as a prerelease with a
different signature; published docs mix the two freely, so don't "correct"
existing code to a signature seen elsewhere without checking which major
version it's from. `BurntSushi/toml v1.6.0`, `golang.org/x/term`. No
external test runner — plain `go test`.
