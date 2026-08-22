# git-sync: Design

## Goal

Keep git repos in sync between two machines (A and B) automatically, using
git hooks, so switching machines mid-work is seamless. Every commit made on
one machine should be pushed to the shared remote and pulled onto the other
machine, without blocking the commit and without requiring manual steps —
including for repos created after the tool is installed.

## Requirements

- Installing the tool must retroactively apply to every existing repo on a
  machine, and automatically apply to every repo created afterward (`git
  init` or `git clone`), with no per-repo setup step.
- Sync must be asynchronous: `git commit` (and the resulting push/notify
  work) must never block or slow down the user's normal git workflow.
- Both machines run identical, symmetric setup — either machine can be the
  one that just committed (pusher) or the one being notified (receiver).
- The peer machine is reachable via direct SSH (already configured, e.g. via
  Tailscale/LAN/keys) using a hardcoded hostname per machine.
- Repos live at the same path relative to `$HOME` on both machines, so a
  repo's identity across machines is just that relative path.
- Receiving an update must never lose local uncommitted work. If the local
  working tree is dirty, stash it, pull, then reapply the stash. If the pull
  can't fast-forward due to diverged history (not a dirty tree), just fetch
  and leave merging to the user.
- Out of scope (YAGNI): retry/queueing for offline pushes, auto-cloning new
  repos onto the peer, auto-resolving real merge/stash conflicts, anything
  beyond existing SSH key auth, non-macOS/Linux support.

## Architecture

Two scripts, installed identically on both machines:

- `~/.gitsync/hooks/post-commit` — installed as the target of git's global
  `core.hooksPath`, so it fires on every commit in every repo on the
  machine, current and future. In the background: pushes the current repo,
  then (on push success) SSHes the peer to invoke its `receive-sync` script
  with this repo's path.
- `~/.gitsync/bin/receive-sync <relpath>` — invoked only remotely, via SSH,
  by the peer's `post-commit`. Applies the incoming changes to the local
  copy of the named repo, handling a dirty working tree by stashing around
  the pull.

Because `receive-sync` never commits or pushes, it cannot re-trigger the
peer's `post-commit` — there is no feedback loop between the two scripts.

## File Layout

Identical on both machines:

```
~/.gitsync/
├── config                  # PEER_HOST=..., PEER_USER=... (hardcoded per machine)
├── hooks/
│   └── post-commit         # global core.hooksPath target
├── bin/
│   └── receive-sync        # invoked remotely via ssh
├── install.sh              # idempotent setup script
└── sync.log                # append-only log of all background activity
```

`config` is a simple shell-sourceable `KEY=value` file:

```
PEER_HOST=other-machine.local
PEER_USER=guillermo
```

## Behavior

### `post-commit` (runs on the machine where the commit happened)

1. Determine the repo root and compute its path relative to `$HOME`. If the
   repo isn't under `$HOME`, log and exit — no sync possible (path identity
   assumption doesn't hold).
2. Fork a background subshell (`( ... ) >>~/.gitsync/sync.log 2>&1 & disown`)
   so the hook returns immediately and the commit is never blocked.
3. In the background: `git push`. On failure (offline, rejected, no
   upstream), log and stop. No retry queue.
4. On push success: `ssh -o ConnectTimeout=5 $PEER_USER@$PEER_HOST
   '~/.gitsync/bin/receive-sync "<relpath>"'`. On SSH failure (peer
   offline/unreachable), log and stop.

### `receive-sync <relpath>` (runs on the machine being notified)

1. `cd ~/<relpath>`. If it doesn't exist, log "unknown repo, skipping" and
   exit — no auto-clone; first-time setup on a new machine is a manual `git
   clone`.
2. `git fetch`.
3. Attempt `git pull --ff-only`.
4. If that fails because the working tree is dirty: `git stash -u && git
   pull --ff-only && git stash pop`. If `stash pop` itself conflicts, leave
   the conflict and the stash in place and log loudly — never auto-resolve
   content conflicts.
5. If the fast-forward fails for a reason other than a dirty tree (i.e.
   diverged history): the fetch in step 2 already updated remote-tracking
   refs; log "diverged, fetched only, manual merge needed" and stop.

### `install.sh` (run once per machine)

Idempotent. Performs:

- `git config --global core.hooksPath ~/.gitsync/hooks`
- `chmod +x` on both scripts
- Writes `~/.gitsync/config` if it doesn't already exist (peer host/user
  supplied as an argument or prompted for)
- A `ssh -o BatchMode=yes` check against the configured peer to confirm
  passwordless SSH auth works, warning (not failing) if it doesn't yet.

## Logging

All background push/pull/stash activity — successes, skips, and failures —
appends to `~/.gitsync/sync.log`. Since none of this can prompt the user
interactively, the log is the only record of what happened and is where a
user investigates when a repo seems out of sync.
