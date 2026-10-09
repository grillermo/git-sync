# ./activate — making synced code live on the receiving machine

Status: design, not implemented.

## Problem

git-sync puts the newest commit on every machine, but for many repos the
newest *code* is not what runs: a Go binary in `bin/` is still the old build,
a skill added to `agents-configs` is not linked into `~/.claude` until
`./install.sh` runs, a personal web service is still serving the old process.
Arriving at another machine means remembering, per repo, which extra step
makes the code active.

## Decision

Each repo that needs a step after new code lands carries one executable,
`./activate`, at its root (next to the existing `build` / `serve`
convention). After a receive moves the checkout forward, git-sync runs it.
A repo without `./activate` needs nothing — that is the whole opt-in, so
repos whose services other people depend on simply never get one.

### Scope

Only the receiving machines run `activate`. The machine that made the
commit is where the user is working; they build/install there by hand.

| Repo | `activate` does |
|---|---|
| `git-sync` | `./build` for this machine only, then copy into `~/.gitsync/bin` (`git-sync service install --local` path or equivalent). `watch` restarts itself on the replaced binary (`ExitRestart`). |
| `currentps`, `gworktree`, `disk-space-differ` | `./build` |
| `top_cpu` | `./build` only. Its running service is not restarted. |
| `powerline` | `./build.sh` (never `./build`, which opens a picker) |
| `zsh` | `programs/pick/rebuild` |
| `agents-configs` | `./install.sh` |
| `file_server`, `ipad-send`, `ntfyllermo`, `serve-html-markdown`, `rulinky` | delegate to `./serve` — only if the service's tmux session exists here |

### Personal services: `activate` delegates to `./serve`

`./serve` is already how a service is registered in
`server/after-reboot.sh`, so it stays the single definition of "run the
latest code of this service". Each of the five gets the same two-line
`activate`:

```sh
#!/bin/sh
# Restart this service on the new code, but only where it already runs.
tmux has-session -t "=<session>" 2>/dev/null || exit 0
exec "$(dirname "$0")/serve"
```

The `has-session` guard keeps a sync to a laptop from starting the service
there (`ipad-send/serve` would otherwise happily start one on any machine).

That puts three requirements on `./serve` itself — the `file_server/serve`
pattern, which `file_server` and `ipad-send` already follow:

1. **Restarts in place and returns.** It (re)spawns the server inside its
   detached tmux session (`respawn-pane -k`) and exits, rather than running
   the server in the foreground. A foreground `serve` under `activate` would
   start a second copy next to the one in tmux, fight it for the port, and
   then be killed by the activate timeout.
2. **No tty, no prompt.** Attaches only when `[ -t 1 ]`, as `file_server`
   does; otherwise prints how to attach and exits 0.
3. **Owns its own preparation.** Whatever the new code needs before it can
   run — `bundle install`, `pnpm install --frozen-lockfile`, `db:migrate`,
   asset builds — happens in `serve`, so a reboot and a sync bring the
   service up the same way. (`rulinky` and `serve-html-markdown` already
   build assets there; neither installs dependencies yet.)
4. **Sets its own PATH** (rbenv, pnpm, bun, Homebrew). The two-line
   `activate` passes on whatever git-sync's environment has, which is not
   a login shell's.

Work per repo: `file_server` and `ipad-send` need only the `activate` file.
`ntfyllermo`, `serve-html-markdown` and `rulinky` run in the foreground
today and have to be converted to the `file_server` shape.
`after-reboot.sh` now runs `./serve` plainly in pane 1 (no wrapper). Resolved:
`~/c/server/serve-in-place.sh` starts each server in its own tmux pane through
`top-cpu-service-wrapper`, so the registry entry `top_cpu` reads names the
live server's PID rather than the `serve` process that returned.

Out of scope, never get an `activate`: `comunidad-antesis`, `readitsoon`
(used by other people; deploys stay manual), `server` (Caddy fronts those
services), `readitsoon-companion` (needs `BASE_URL`, rarely changes),
`chicle`, `readitsoon-mcp`, `readitsoon-obsidian-plugin` (nothing to run).
Already self-activating: `claude-sessions` (wrapper rebuilds when source is
newer), `claude-swap` once installed with `pipx install -e ~/c/claude-swap`
(one-time manual fix: today `cswap` is the PyPI release, not this repo).

## The `activate` contract

1. **Executable, idempotent, non-interactive.** Runs with no tty and stdin
   at `/dev/null`. Re-running with nothing new is harmless.
2. **Decides for itself whether this machine applies.** A service restart
   is guarded by `tmux has-session -t =<name> 2>/dev/null || exit 0`, so it
   only fires where the service already runs (the Mac mini), and never
   starts one anywhere new. No host lists.
3. **Sets its own PATH.** git-sync runs it from an ssh non-login command or
   from launchd, neither of which loads `~/.zshrc`. Each script prepends
   what it needs (`/opt/homebrew/bin`, `$HOME/go/bin`/Go, rbenv shims,
   `$PNPM_HOME`, bun). Without this the likely failure is a silent
   `go: not found`.
4. **Environment it receives:**
   - `GITSYNC_OLD_REV`, `GITSYNC_NEW_REV` — HEAD before and after the
     fast-forward, so a script can skip work (e.g. only docs changed) with
     `git diff --quiet "$GITSYNC_OLD_REV" "$GITSYNC_NEW_REV" -- <paths>`.
   - `GITSYNC_FROM` — the machine that notified.
   - `GITSYNC_INTERNAL=1` is *not* set: activate is user code, and a commit
     it made would be a real commit. (It should not commit anything.)
5. **Exit status is the result.** 0 = activated or nothing to do; non-zero
   = failed. Output (stdout+stderr) goes to the log, not a terminal.

## git-sync changes

- **When it runs.** In `syncRepo`, record `HEAD` before the merge; if the
  fast-forward succeeded and `HEAD` moved, and `<dir>/activate` is an
  executable regular file, **enqueue** the repo (below) and spawn a drainer.
  This covers `receive` and `announce`'s catch-up (which calls `receive`).
  Not run when the merge failed (diverged), when HEAD did not move, or on
  the pushing machine. `initialsync` fast-forwards that land on a peer
  enqueue too, through the same entry point over ssh.

### One machine-wide queue, drained serially

At most one `activate` runs on a machine at a time, across all repos.
`announce` on wake can fast-forward a dozen repos at once; running a dozen
Go builds, `install.sh` and Rails asset compiles in parallel would swamp
the machine and interleave service restarts.

- **Queue.** One file per repo under `~/.gitsync/activate/queue/`, named by
  the path-escaped rel (same escaping as `pending/`). Its content is the
  `OLD_REV` — HEAD before the first sync not yet activated. Created
  exclusively (`O_EXCL`, written via temp + `link`) so it is atomic with no
  lock, like the pending markers. **Enqueueing a repo that is already queued
  is a no-op**: the existing entry keeps the older `OLD_REV`, so it covers
  every commit since the last activation. A repo is in the queue at most
  once — that is the coalescing: three quick commits = one build.
- **Order.** FIFO by the entry file's mtime (enqueue time).
- **Drainer.** Hidden subcommand `git-sync activate --drain`, spawned
  detached (`detachAttr()`, like `hook` → `push`) by whoever enqueued,
  *after* the receive lock is released — activation can take minutes, and
  holding the receive lock that long would refuse the user's commits and
  keep the pusher's ssh notify waiting. The drainer takes one global lock
  (`locks/_activate`, via `internal/lock`, restamped by the same
  `heartbeat` as receive, since a run can outlast `StaleAfter`). If the
  lock is busy it exits at once: the live drainer will pick the entry up.
- **Draining.** Loop: take the oldest entry, *remove it first*, then run
  `<dir>/activate` with `GITSYNC_OLD_REV` from the entry and
  `GITSYNC_NEW_REV` = HEAD read *now*. Removing before running means a sync
  that lands during the run re-enqueues the repo, so it activates again
  afterwards, rather than being swallowed by the run already in progress.
  Repeat until the queue is empty.
- **No lost wake-up.** After releasing the lock, the drainer checks the
  queue once more and, if an entry appeared in the gap (its spawned drainer
  saw the lock busy and left), re-acquires and keeps going.
- **Recovery.** `watch` (at login and on wake) spawns a drainer when the
  queue is non-empty, so an entry left by a crashed or killed drainer runs
  later. Entries for repos no longer selected, or whose `activate` is gone,
  are dropped with a `skip` event.
- **Repo lock while running.** The drainer takes the repo's receive lock
  while its `activate` runs, so a receive cannot fast-forward the tree
  under a build. A receive that finds it busy skips as it does today; the
  next commit or `announce` brings it up to date.
- **Logging.** New activity op `OpActivate` with ok/warn/error events
  (`"activated in 12s"`, `"activate exited 1 — see activate/<rel>.log"`),
  so `report` shows it. Full output appended to
  `~/.gitsync/activate/<rel-escaped>.log`, truncated to the last N runs.
- **Timeout.** Killed after a generous ceiling (default 15 min,
  `GITSYNC_ACTIVATE_TIMEOUT` escape hatch for tests), logged as error.
- **Failure policy.** A failed activate is a `warn`/`error` event, never
  retried automatically and never rolled back — same stance as a rejected
  push: it needs a human, and the next commit to that repo tries again.
- **Manual entry point.** `git-sync activate <repo>` from a terminal waits
  for the global lock (so it never runs beside a drainer), then runs it in
  the foreground with output shown, for re-trying by hand.
- **Machine off.** An activation that never ran because the machine was
  off needs no special handling: `announce`'s catch-up receive moves HEAD
  and enqueues then.

## Open shells (`zsh`, `claude-menu`)

Already-open shells cannot be reached from outside, and `activate` does not
try. Instead `~/c/zsh/init` records, at load, the HEAD of `~/c/zsh` and
`~/c/claude-menu`; a `precmd` hook (cheap: one `stat` of each repo's
`.git/HEAD`-resolved ref file, not a `git` exec per prompt) prints once:

    zsh config changed since this shell started — run `exec zsh`

It never re-sources automatically.

## Cleanups found during the survey

- `powerline/build.sh` reads `HOSTNAME_COLOR` from `.env`, but `./build`
  records the picked color in `color.env`; a non-interactive rebuild loses
  the color. `build.sh` should fall back to `color.env`.
- `~/.local/bin/currentps` is a dangling symlink to `~/c/currentps/currentps`.
- `zsh/programs.zsh` refers to `~/c/gworktree/rebuild`; the script is `./build`.
- `~/.zshrc` adds `~/c/git-sync` and `~/c/claude-sessions` to PATH, against
  the `programs.zsh` rule; the git-sync entry points at a directory with no
  binary in it.

## Testing

- `syncer` unit tests in a `NewSandbox`: activate spawned only when HEAD
  moved; not on diverged / no-op / missing or non-executable file; receives
  the right env; receive lock already released when it runs.
- Queue: enqueue is idempotent and keeps the oldest `OLD_REV`; drain order
  is FIFO; two drainers never run `activate` concurrently (a test activate
  that records start/end times shows no overlap, under `go test -race`).
- A sync landing mid-run re-enqueues and activates once more after.
- Lost wake-up: an entry added between the last check and the release is
  still drained.
- Stale entry for an unselected repo or a missing `activate` is dropped.
- Timeout and non-zero exit → error event, log file written.
- e2e: a commit on machine A to a repo whose `activate` writes a marker file
  shows the marker on machine B, and not on A.
