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
  working tree is dirty, stash it before pulling and reapply the stash
  afterward regardless of whether the pull succeeded or the history had
  diverged — a dirty tree must never end up silently hidden in the stash.
  If history can't fast-forward (diverged, independent of tree dirtiness),
  just fetch and leave merging to the user.
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
2. Background the rest of the work with `nohup bash -c '...' </dev/null
   >>~/.gitsync/sync.log 2>&1 &`, so the hook returns immediately and the
   commit is never blocked. `nohup` + redirected stdio is used instead of
   `disown` because hooks run non-interactively, where shell job control
   (which `disown` depends on) is not reliably enabled.
3. In the background: `git push`. On failure (offline, rejected, no
   upstream), log and stop. No retry queue.
4. On push success: `ssh -o ConnectTimeout=5 -o BatchMode=yes $PEER_USER@$PEER_HOST
   '~/.gitsync/bin/receive-sync "<relpath>"'`. On SSH failure (peer
   offline/unreachable), log and stop.

### `receive-sync <relpath>` (runs on the machine being notified)

1. `cd ~/<relpath>`. If it doesn't exist, log "unknown repo, skipping" and
   exit — no auto-clone; first-time setup on a new machine is a manual `git
   clone`.
2. **Serialize concurrent runs against the same repo.** Rapid consecutive
   commits on the pusher can fire overlapping `receive-sync` invocations for
   the same repo, which would race their `stash`/pull/`stash pop` steps
   against each other. Acquire a per-repo lock before doing anything else:
   atomically `mkdir` a lock directory under `~/.gitsync/locks/<relpath
   with / replaced by _>.lock`, retrying with a short backoff for up to 30s.
   If still locked after 30s, log "sync already in progress, giving up" and
   exit — the run that holds the lock will bring the repo fully up to date
   anyway, since git fetch/pull/stash operations are idempotent against
   whatever state currently exists. Release the lock (`rmdir`) on every exit
   path, including failures. If the lock directory is older than 5 minutes
   when a new run tries to acquire it, treat it as stale (left behind by a
   killed process, e.g. an SSH drop or `kill -9`) and remove it before
   retrying, rather than waiting out the full 30s and giving up.
3. Determine the currently checked-out branch with `git symbolic-ref --short
   HEAD`. If HEAD is detached (no branch), log "detached HEAD, skipping"
   and exit — out of scope for v1.
4. `git fetch`.
5. Check whether the working tree is dirty with `git status --porcelain`
   (not by inspecting `git pull`'s exit code, which can't distinguish a
   dirty tree from diverged history). If dirty, `git stash -u`.
6. Attempt `git merge --ff-only "@{upstream}"` for the checked-out branch.
   - **Success:** if step 5 stashed, `git stash pop`. If the pop itself
     conflicts, leave the conflict and the stash entry in place and log
     loudly — never auto-resolve content conflicts.
   - **Failure (history diverged, not fast-forwardable):** the fetch in
     step 4 already updated remote-tracking refs. If step 5 stashed,
     `git stash pop` to restore the original working tree exactly as it was
     (this always runs, so a dirty tree is never left hidden in the stash
     regardless of why the merge failed). Log "diverged, fetched only,
     manual merge needed" and exit.

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

## Known Limitations

- **`core.hooksPath` is global and exclusive.** Setting it points git at
  `~/.gitsync/hooks` for *every* hook type in *every* repo on the machine,
  replacing (not chaining with) any repo-local hooks (e.g. from Husky or
  `pre-commit`) that a project might otherwise rely on. If a repo needs its
  own hooks alongside git-sync, `post-commit` must manually invoke them —
  not handled in v1; if this matters for a specific repo, add that chaining
  by hand.
- **Syncs whatever branch is checked out on the receiver**, using that
  branch's own upstream — not necessarily the branch that was just pushed
  on the pusher. If the two machines have different branches checked out,
  the receiver's `merge --ff-only` will simply have nothing to fast-forward
  into and no-op; this is treated as normal (not an error), since the
  receiver's own branch has nothing new coming from a different branch.
