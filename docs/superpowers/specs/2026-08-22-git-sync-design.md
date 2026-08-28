# git-sync: Design

## Goal

Keep a chosen set of git repos in sync between two machines (A and B)
automatically, so switching machines mid-work is seamless. Every commit made
on one machine should be pushed to the repo's shared git remote and pulled onto the
other machine from that same remote, without blocking the commit and without
requiring manual steps. The remote is the transport; SSH between the machines
carries only the nudge to pull.

A single command, run once on one machine, installs it on both, picks the
repos and configures the pair. A single command shows what it has been doing,
and a single command removes it.

## Requirements

- **The shared git remote is the transport.** No repository data ever moves
  machine to machine: a commit is pushed to the repo's remote, and the peer
  pulls it back down from that same remote. SSH carries one thing only — the
  notification that there is something to pull. Both machines therefore need
  push access to that remote.
- **The remote is resolved by name, per repo, by the same rule on both
  machines:** `github` if the repo has a remote called that, else `origin`,
  else the repo's single remote if it has exactly one. The preference order is
  `remote_names` in `config.toml`. Several remotes and none of them named in
  the list is an ambiguity git-sync refuses to guess at. The branch's
  `@{upstream}` is deliberately *not* consulted: a branch can track
  `origin/main` in a repo whose shared remote is `github`, and the two
  machines must arrive at the same answer from the same rule.
- A repo with no such remote cannot sync, and says so rather than failing
  silently: the picker marks it, and a commit in one records a warning.
- Sync is opt-in per repo. `install` scans the configured root, presents the
  repos it finds, and syncs only the ones the user selects. A repo that was
  not selected is inert: committing in it does nothing.
- Choosing repos must not be a per-repo chore. One pass through one checkbox
  list at install time covers every repo on the machine, and re-running
  `install` on the same root reopens that list to amend the choice.
- `install` runs on one machine only. It sets that machine up and then
  provisions the peer over SSH — binary, config, hook and `core.hooksPath` —
  so the second machine needs no commands typed on it at all. The machine
  where `install` ran owns the decisions; the peer follows them.
- Re-running `install` must make newly appeared repos easy to spot: they are
  listed first, and the repos already being synced are listed after them,
  pre-ticked.
- Sync must be asynchronous: `git commit` (and the resulting push/notify
  work) must never block or slow down the user's normal git workflow.
- Once installed, both machines are symmetric at runtime — either can be the
  one that just committed (pusher) or the one being notified (receiver). The
  asymmetry is only in setup: one machine installs, the other is provisioned.
- The peer machine is reachable via direct SSH (already configured, e.g. via
  Tailscale/LAN/keys) using a hardcoded hostname per machine.
- **SSH must never need a human at sync time.** Key auth is the smooth path,
  but a peer that answers with a password prompt has to work too: `install`
  asks for the password once, on the terminal the user is already looking at,
  verifies it against the peer, and stores it in the OS keychain. Every later
  sync reads it from there through an askpass helper. This is not a
  convenience — the hook runs detached with no terminal, so a prompt at sync
  time cannot be answered by anyone and the sync would simply fail forever.
- The password is stored in the OS keychain and nowhere else: not in
  `config.toml`, not in `debug.log`, and never as a command-line argument,
  where `ps` would show it to anyone on the machine. An unverified password is
  never stored — one that was wrong would fail every later sync silently.
- A repo's identity across machines is its path relative to the configured
  sync root (`base_dir`). The two machines need not use the same absolute
  `base_dir`, but a synced repo must sit at the same path beneath it on both,
  and both copies must point at the same remote — two clones of *different*
  repositories at the same relative path would never converge.
- After the repos are chosen, `install` must verify against the peer that each
  one exists there and points at the same remote, and report every mismatch.
  The picker can only see the machine it runs on, so without this check a
  one-sided or differently-remoted repo is selected happily and then never
  syncs, with nothing to show for it. The user can quit at that point — with
  `q`, as in the picker — and nothing has been written on either machine.
- **Every sync targets the remote's default branch, resolved per repo** —
  `main`, `master`, `trunk`, whatever `git remote set-head` resolves the
  remote's `HEAD` to — not whatever branch happens to be checked out. Push,
  receive and install's initial sync all anchor to it, independent of a
  feature branch or detached HEAD on either machine. A commit on any other
  branch simply leaves the default branch unmoved, which is what makes
  "non-default branches never sync" true without a separate guard: there is
  nothing new to push or receive. A repo with no local default branch (only
  feature branches, nothing named `main`/`master`/`trunk` locally) is
  skipped and reported; a branch is never auto-created or checked out to
  make one exist.
- Receiving an update must never lose local uncommitted work. If the local
  working tree is dirty *and the default branch is what's checked out*,
  stash it before pulling and reapply the stash afterward regardless of
  whether the pull succeeded or the history had diverged — a dirty tree must
  never end up silently hidden in the stash. When the default branch is not
  the checked-out branch, there is no working tree to disturb: the local ref
  is simply advanced directly. If history can't fast-forward (diverged,
  independent of tree dirtiness), `receive` just fetches and leaves merging
  to the user — steady-state `receive` never auto-merges, in either case.
  The one exception to "never auto-merge" is install-time only (below).
- A repo that exists on only one machine must be a silent, harmless no-op —
  not an error, and not something that pollutes the record with failures.
- Everything the tool does must be inspectable after the fact. None of this
  work can prompt the user or print to a terminal they are watching, so the
  activity record is the only account of what happened.
- Out of scope (YAGNI): retry/queueing for offline pushes, auto-cloning new
  repos onto the peer, auto-resolving real merge/stash conflicts, generating
  or installing SSH keys on the user's behalf, interactive keyboard-interactive
  or 2FA SSH flows, syncing more than two machines, non-macOS/Linux support.

## Architecture

One statically linked Go binary, installed identically on both machines, with
six subcommands. Three are for people:

- `git-sync install <base_dir>` — idempotent setup for *both* machines, run
  on one of them. Scans `base_dir` for git repos, opens a checkbox list to
  choose which to sync, points git's global `core.hooksPath` at git-sync's
  hook, then pushes the same binary, repo selection and hook to the peer over
  SSH. The hook fires on every commit in every repo on a machine; it syncs
  only the selected ones.
- `git-sync report` — an interactive TUI over the activity record, grouped by
  repo. Prints a static, greppable report instead when its output is not a
  terminal.
- `git-sync uninstall` — removes the hook and the installed binary.

Three are invoked by machines, never typed by a human:

- `git-sync hook post-commit` — what git runs after every commit. Identifies
  the repo and hands off; does nothing else.
- `git-sync push <repo>` — runs detached in the background: pushes the
  remote's default branch (not the currently checked-out branch) to the
  repo's resolved remote, then (on push success) SSHes the peer to invoke
  its `receive`.
- `git-sync receive <repo>` — invoked only remotely, over SSH, by the peer's
  `push`. Fetches the same remote and fast-forwards the local copy of the
  default branch onto it — stashing around the merge if it's the
  checked-out branch and dirty, or advancing the ref directly with no stash
  if it isn't checked out at all.

Because `receive` never commits and never pushes, it cannot re-trigger the
peer's hook — there is no feedback loop between the two machines.

## File Layout

Identical on both machines, created by `install`:

```
~/.gitsync/
├── bin/git-sync         # the installed binary; what the hook and ssh invoke
├── hooks/post-commit    # global core.hooksPath target; execs the binary
├── askpass              # SSH_ASKPASS helper; prints the peer's stored password
├── config.toml          # base_dir, peer_host, peer_user, the selected repos
├── activity.jsonl       # structured event record; the report's data source
├── debug.log            # raw git and ssh output, for troubleshooting
└── locks/               # per-repo locks
```

`GITSYNC_HOME` overrides `~/.gitsync`.

`config.toml` is hand-editable:

```toml
base_dir  = "/Users/guillermo/code"
peer_host = "other-machine.local"
peer_user = "guillermo"

# The repos to sync, as paths relative to base_dir. Chosen through the
# checkbox list during install; editable by hand.
repos = [
  "notes",
  "work/api",
  "work/web",
]

# The remote each repo syncs through, in preference order: the first of these
# names the repo actually has wins, falling back to its single remote if it
# has exactly one. This is the transport - the commits travel through it.
remote_names = ["github", "origin"]
```

`repos` is the allowlist. Nothing outside it is ever pushed or received, and
it is the one place that decides what git-sync touches. Both machines hold
the same list: `install` writes it on the machine it runs on and copies it to
the peer, so the two never disagree about what is being synced.

The peer's `config.toml` is the mirror image — same `repos`, its own
`base_dir`, and `peer_host`/`peer_user` pointing back at the installing
machine.

`hooks/post-commit` is a shell shim rather than the binary itself, so that
re-installing a new binary cannot race a commit that is executing the old one:

```sh
#!/bin/sh
exec "/Users/guillermo/.gitsync/bin/git-sync" hook post-commit
```

## Behavior

### `install <base_dir>`

Idempotent; re-run any time to amend the repo selection or update the
installed binary.

It runs as a four-stage wizard, and the order matters: **connect → pick →
verify → install**. Nothing outside this machine is written before the last
stage, and quitting at any question leaves the peer untouched.

1. **Connect.** Establish that SSH to the peer works unattended, asking for
   and saving a password if that is what the peer requires (below). First,
   because every later stage depends on it — and because discovering the peer
   wants a password *after* the user has ticked forty repos would mean asking
   for both again.
2. **Pick.** Scan and open the repo picker.
3. **Verify.** Ask the peer whether the chosen repos line up, and report
   mismatches.
4. **Install.** Write this machine, then provision the peer.

In detail, it:

- Resolves `base_dir` to an absolute path and verifies it exists — the hook
  runs from arbitrary working directories, so a relative root is meaningless
  once stored.
- **Ensures SSH works without a terminal** (below), prompting for the peer's
  password once and saving it to the OS keychain if key auth is not in place.
  The saved password is read back and confirmed before the wizard moves on: a
  keychain write can report success without persisting, and the cost of
  believing it lands on the next commit, in the background, where nobody is
  watching.
- **Scans `base_dir` for git repos.** A directory containing `.git` (a
  directory or a file, so linked worktrees and submodules count) is a repo,
  and the scan does not descend into one once found — a repo's own contents
  cannot hold a separately syncable repo. Directories whose names begin with
  `.` are not descended into either.
- **Opens the repo picker** (below) and writes the chosen set to `repos`.
- **Checks the chosen repos against the peer** (below) before writing
  anything, and reports every mismatch.
- Copies the running binary to `~/.gitsync/bin/git-sync` (write to a temp file
  and rename, so a concurrent hook never executes a half-written binary), and
  writes the hook shim.
- Writes `config.toml`. Peer host and user come from `--peer-host` /
  `--peer-user`, falling back to an existing config, then to prompting.
- Sets `git config --global core.hooksPath ~/.gitsync/hooks`.
- **Provisions the peer over SSH** (below), so nothing has to be typed on the
  second machine.

Existing `activity.jsonl` is never touched. Cancelling the picker, or quitting
at the peer check, cancels the whole install: nothing is installed and the
peer is not touched. The one thing that survives is a password saved in stage
1 — it had to be saved to get as far as the question being quit at — and the
cancellation message says so. `uninstall --purge` forgets it.

#### SSH authentication

Checked before anything else touches the peer, because everything after it
depends on SSH working without a human:

1. Probe the peer with `BatchMode=yes`. If it succeeds, keys work and nothing
   is asked — the common case, and worth staying quiet about.
2. SSH exits 255 both for "cannot connect" and for "wants a password", so the
   two are told apart by the message (`Permission denied`,
   `Authentication failed`). Only the second is answerable.
3. On a terminal, prompt for the password without echoing it, store it, probe
   again, and keep it only if that probe succeeded — otherwise remove it and
   re-prompt, up to three times. Storing before testing is unavoidable (the
   askpass helper reads from the store), so removing on failure is what keeps
   the invariant that a stored password is a working one.
4. Without a terminal, stop and say what to do — set up a key, or run
   `install` interactively — rather than hanging on a prompt no one can see.

Every later SSH invocation, from both `install` and `push`, is built the same
way: with no stored password, `BatchMode=yes`, so a failure is immediate
rather than a hang; with one, `BatchMode` off, `SSH_ASKPASS` pointing at
`~/.gitsync/askpass` and `SSH_ASKPASS_REQUIRE=force`, so ssh takes the
password from the helper instead of a terminal. The helper is a shim with the
account baked in, since `SSH_ASKPASS` names one executable and takes no
arguments of its own.

Runtime is symmetric, so the way back matters as much: if this machine also
requires a password, the peer needs one stored too, or syncing works in one
direction only. Provisioning offers to reuse the password just typed and
sends it to the peer over the already-encrypted SSH channel, on stdin, to be
stored in the peer's keychain — never as part of the remote command.

#### Checking the chosen repos against the peer

Runs after the picker and before anything is written, on one SSH round trip
for all repos at once. For each selected repo it asks the peer whether the
path holds a git repo and, if so, which remote URL it resolves to by the same
`remote_names` rule this machine uses. Each repo comes back as one of:

| State | Meaning |
|---|---|
| `present` | a clone of the same remote; it will sync |
| `missing` | nothing at that path; it will never sync |
| `not-a-repo` | a directory, but not a clone |
| `no-remote` | cloned, but with no remote to sync through |
| `other-remote` | a clone of a *different* repository; the two would never converge |
| `unchecked` | the peer could not be asked about this one |

Mismatches are listed with both remote URLs where they differ, followed by
what to do about it. On a terminal the user is then asked whether to go on:
`q` quits with nothing written on either machine. Without a terminal it
reports and continues, since there is no one to ask. A mismatch never blocks
the install by itself — setting up a repo you are about to clone is
legitimate — and an unreachable peer downgrades the check to a warning rather
than a dead end.

#### Provisioning the peer

Runs after the local install has succeeded, over the already-configured SSH
connection. In order:

1. `ssh peer 'uname -sm'` and compare with this machine's OS and architecture.
   The binary is copied verbatim, so a mismatch means it cannot run there —
   report it and skip the rest of the provisioning rather than installing
   something broken. The local install stands.
2. `ssh peer 'echo $HOME'` to learn the peer's home directory, so every path
   written to the peer is absolute and nothing depends on shell expansion
   later.
3. Create `~/.gitsync/{bin,hooks,locks}` on the peer.
4. Stream the binary over SSH to a temp path, `chmod +x`, then rename into
   place — the same write-then-rename as locally, so a commit running on the
   peer never executes a half-copied binary.
5. Write the peer's `config.toml`: the same `repos`, the peer's own
   `base_dir`, and `peer_host`/`peer_user` pointing back at *this* machine.
6. Write the peer's hook shim and `chmod +x` it.
7. `ssh peer 'git config --global core.hooksPath <peer_home>/.gitsync/hooks'`.

The peer's `base_dir` defaults to the same path relative to `$HOME` as this
machine's — `/Users/me/code` here becomes `$HOME/code` there — and
`--peer-base-dir` overrides it when the layouts differ.

The peer needs to reach back to *this* machine, which means knowing this
machine's hostname as the peer sees it. That is taken from the system hostname
and overridden with `--self-host`. It is the one value `install` cannot
discover reliably, so it is reported in the output for confirmation.

If the peer is unreachable, `install` completes the local half, says clearly
that the peer was not provisioned, and exits successfully — re-running once the
peer is up finishes the job. Provisioning is idempotent for the same reason
the local install is.

`--no-peer` skips provisioning entirely, for setting up a machine before its
peer exists.

#### The repo picker

A checkbox list, ordered so that what changed since last time is what you see
first:

```
  SELECT REPOS TO SYNC                    /Users/guillermo/code

  NEW
  [ ] experiments/raytracer   no remote - cannot sync
  [ ] work/billing            origin   203 commits   last commit 3d ago

  ALREADY SYNCING
  [x] notes                   github   891 commits   last commit 20m ago
  [x] work/api                github   142 commits   last commit 1h ago
  [x] work/web                origin    38 commits   last commit 5d ago

  [space] toggle   [a] all   [n] none   [enter] save   [q] cancel
```

- **NEW** holds repos found by the scan that are not currently in `repos`.
  They come first and start unticked, so adopting one is a deliberate act.
  On a first install every repo is new, and the section header is omitted.
- **ALREADY SYNCING** holds the repos currently in `repos`, pre-ticked.
  Unticking one stops syncing it.
- A repo in `repos` that the scan no longer finds on disk is listed in a
  third **MISSING** section, pre-ticked, so that a repo on a temporarily
  unmounted volume is not silently dropped from the config by an unrelated
  re-run. Unticking it removes it.

Each row shows the repo's path relative to `base_dir`, the remote it would
sync through, its commit count and how long ago its last commit was, so the
choice can be made without leaving the picker. A repo with no remote is marked
as unable to sync; it stays tickable, because `git remote add` is the fix and
the user may be about to do exactly that.

When stdout is not a terminal the picker cannot run, and `install` requires
the selection up front instead: `--all` selects every repo the scan finds,
and `--repos a,b,c` selects exactly those. Passing either flag skips the
picker even on a terminal. Without a terminal and without either flag,
`install` exits with a usage error rather than hanging.

### `uninstall [--purge]`

- Unsets the global `core.hooksPath`, but only if it still points at
  git-sync's hooks directory. Another tool may own it by now, and clobbering
  that would break their setup.
- Removes the hook, the installed binary, and the locks.
- Keeps `config.toml` and `activity.jsonl`, so `git-sync report` still works
  on the history afterward. `--purge` removes those too, and forgets the
  peer's stored password.

Running it on a machine where nothing is installed is a no-op, not an error.

### `hook post-commit` (runs on the machine where the commit happened)

git waits for this process, and reads its stdout, so it must do almost
nothing.

1. Load the config. If there is none, do nothing — an uninstalled or
   half-installed git-sync must never break a commit.
2. Determine the repo root and compute its path relative to `base_dir`. If the
   repo isn't under `base_dir`, record a skip and exit; a repo outside the
   sync root has no cross-machine identity to act on.
3. If the repo is not in `repos`, exit without recording anything. The hook
   fires in every repo on the machine, so an unselected repo is the common
   case, not an event — recording it would bury the real activity under noise.
4. Start `git-sync push <repo>` in its own session (`setsid`), with its stdio
   pointed at `debug.log`, and return immediately without waiting for it.
   Both halves matter: the child must outlive this process, and it must not
   hold git's inherited stdout open, or the commit stalls for as long as the
   push takes.

The hook never fails a commit. Anything that goes wrong is recorded and
swallowed.

### `push <repo>` (runs detached, in the background)

1. Resolve the repo's remote by the `remote_names` rule. If there is none,
   record a warning — the repo is selected, so the user believes it is
   syncing and it is not — and stop without notifying the peer.
2. Resolve `<branch>` as **the remote's default branch**
   (`gitcmd.DefaultBranch`), not the currently checked-out branch — a feature
   branch or a detached HEAD no longer matters here. If there is no local
   branch of that name, record a skip: no local default branch means nothing
   to push, and one is never auto-created.
3. `git push <remote> <branch>`, naming both explicitly rather than relying on
   the branch's upstream, which may point at a different remote. A commit
   that only touched a non-default branch simply leaves `<branch>` unmoved,
   so this is a harmless no-op push ("Everything up-to-date") — this is the
   entire mechanism behind "non-default branches never sync," with no
   separate guard needed. On failure (offline, rejected), record an error and
   stop. No retry queue: if we are offline or the push is rejected, the next
   commit pushes both commits anyway.
4. On push success: `ssh -o ConnectTimeout=5 -o BatchMode=yes
   $peer_user@$peer_host 'git-sync receive <repo>'` — built the same way every
   other SSH call is, so `BatchMode` gives way to the askpass helper when a
   password is stored (see SSH authentication).
5. Record the outcome according to the exit status SSH brings back:
   - `0` — the peer synced.
   - `3` — the peer has no copy of this repo. Recorded as a skip, not a
     success and not an error (see Exit Codes below).
   - `255` — SSH's own code for "could not connect": the peer is unreachable,
     or a stored password has stopped working.
   - anything else — the peer's `receive` failed.

### `receive <repo>` (runs on the machine being notified)

1. If the repo is not in this machine's `repos`, record a skip and exit `3`.
   `install` keeps both machines' lists in step, so this normally cannot
   happen — it catches the cases where they have drifted (a hand-edited
   config, or a peer provisioned before a later selection change) and turns
   the disagreement into the pusher's existing harmless no-op rather than a
   surprise sync.
2. Resolve the repo under the local `base_dir`. If it isn't a git repo,
   record a skip and exit `3` — no auto-clone; first-time setup on a new
   machine is a manual `git clone` followed by selecting it in `install`. This
   is checked by stat-ing `.git` rather than requiring a directory, since
   `.git` is a file in linked worktrees and submodules and those are still
   real repos.
3. **Serialize concurrent runs against the same repo.** Rapid consecutive
   commits on the pusher can fire overlapping `receive` invocations for the
   same repo, which would race their stash/pull/stash-pop steps against each
   other. Acquire a per-repo lock before doing anything else: atomically
   `mkdir` a lock directory under `~/.gitsync/locks/<repo path with / replaced
   by _>.lock`, retrying with a short backoff for up to 30s. A directory is
   used because `mkdir` is atomic on every filesystem we care about, unlike
   check-then-create on a file. If still locked after 30s, record "sync
   already in progress, giving up" and exit — the run that holds the lock will
   bring the repo fully up to date anyway, since git fetch/pull/stash
   operations are idempotent against whatever state currently exists. Release
   the lock on every exit path, including failures. If the lock directory is
   older than 5 minutes when a new run tries to acquire it, treat it as stale
   (left behind by a killed process, e.g. an SSH drop or `kill -9`) and remove
   it before retrying, rather than waiting out the full 30s and giving up.
4. Resolve the repo's remote by the same `remote_names` rule the pusher used.
   If there is none, record a warning and exit: there is nothing to sync
   through. Resolving by name rather than following this branch's
   `@{upstream}` is what keeps the two machines meeting at the same place —
   an upstream pointing at a different remote would have us fetching a
   repository the peer never wrote to and reporting "up to date" forever.
5. Resolve `<branch>` as **the remote's default branch**, the same way `push`
   does — never the locally checked-out branch, and never affected by a
   detached HEAD. If there is no local branch of that name, record a skip:
   nothing to receive onto, and one is never auto-created.
6. `git fetch <remote>`.
7. Verify `<remote>/<branch>` exists. This is checked separately, before the
   merge, because a missing remote-tracking ref would otherwise fail the merge
   and be misreported as diverged history. It is normal when the default
   branch was renamed or removed on the remote since the last clone.
8. Measure ahead/behind between `<branch>` and `<remote>/<branch>`. If
   already level, record up to date and stop.
   - **Diverged (ahead and behind both > 0):** the fetch in step 6 already
     updated remote-tracking refs, so the user has everything they need
     locally to merge by hand. Record "diverged, fetched only, manual merge
     needed" and stop. `receive` is fast-forward-only, always, in steady
     state — it never attempts a merge itself, whether or not `<branch>` is
     checked out. Auto-merging here would let both machines mint rival merge
     commits and re-diverge forever, and `receive` never pushes, so it could
     not fix that itself. (Contrast with `install`'s initial sync, which
     *may* auto-merge a clean divergence once — see below.)
   - **Purely behind:** fast-forward, branching on whether `<branch>` is the
     checked-out branch here:
     - **Checked out:** check whether the working tree is dirty with
       `git status --porcelain` (not by inspecting a pull's exit code, which
       can't distinguish a dirty tree from diverged history). If dirty,
       `git stash push -u`, then `git merge --ff-only <remote>/<branch>`
       (guaranteed to succeed — divergence was already ruled out above), then
       `git stash pop` **unconditionally**, whether or not the merge
       succeeded, so a dirty tree is never left hidden in the stash. If the
       pop itself conflicts, leave the conflict and the stash entry in place
       and record it loudly; never auto-resolve content conflicts.
     - **Not checked out:** advance the local ref directly
       (`git update-ref refs/heads/<branch> <remote>/<branch>`) without
       touching the working tree at all — there is no checkout to disturb,
       so there is nothing to stash.

### `report [flags]`

Reads `activity.jsonl` and groups it by repo, most recently active repo first
— almost always the one you opened the report to look at.

Interactive when stdout is a terminal: a repo list on the left showing each
repo's event count and problem count, that repo's full history on the right,
arrow keys to move between repos, `e` to filter to problems only, `q` to quit.

When stdout is not a terminal, or `--plain` is passed, it prints a static
report instead — a totals line, a per-repo summary table, then a block of
detail per repo, one event per line — so it stays greppable and scriptable.

Flags: `--since <duration>`, `--repo <substring>`, `--errors`, `--plain`.

## Logging

Two records, split by audience.

`activity.jsonl` is append-only, one JSON object per line, and is the only
data source for `report`:

```json
{"ts":"2026-08-22T14:02:11Z","repo":"group/proj","op":"push","status":"ok","msg":"pushed main to github","branch":"main"}
```

`op` is one of `hook`, `push`, `notify`, `receive`. `status` is one of:

- `ok` — it worked.
- `skip` — deliberately did nothing. Routine, not a problem: a repo the peer
  lacks, a detached HEAD, nothing to fast-forward.
- `warn` — needs a human eventually: diverged history, a conflicting stash pop.
- `error` — it failed.

Lines are kept under 4096 bytes (truncating the message if necessary) so that
a single append is atomic under the POSIX `PIPE_BUF` guarantee. Several pushes
and receives append concurrently, and this is what lets them do so without a
lock. A torn line from a killed process is skipped on read rather than
failing the whole history.

`debug.log` collects raw git and ssh output for when the structured record
isn't enough to explain a failure.

## Exit Codes

The binary's exit codes are part of its contract, because SSH carries
`receive`'s status back to the pushing machine:

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | failure |
| 2 | usage error |
| 3 | `receive` only: this machine has no copy of that repo |

Code `3` is what keeps the two machines' records honest. Without it, SSH
returns 0 for a repo the peer has never cloned and the pusher records a sync
that never happened.

## Known Limitations

- **`core.hooksPath` is global and exclusive.** Setting it points git at
  `~/.gitsync/hooks` for *every* hook type in *every* repo on the machine,
  replacing (not chaining with) any repo-local hooks (e.g. from Husky or
  `pre-commit`) that a project might otherwise rely on. If a repo needs its
  own hooks alongside git-sync, the shim must manually invoke them — not
  handled in v1; if this matters for a specific repo, add that chaining by
  hand.
- **Only the remote's default branch ever syncs.** Push, receive and
  install's initial sync all anchor to it (`gitcmd.DefaultBranch`, resolved
  per repo — `main`, `master`, `trunk`, whatever the remote's `HEAD` says),
  regardless of what either machine has checked out — a feature branch or a
  detached HEAD is fine on either side, and syncing continues underneath it.
  Commits on any other branch simply don't sync: there is nothing to push or
  receive on that branch, by design (not an error). A repo with no local
  default branch (only feature branches, nothing named `main`/`master`/
  `trunk` locally) is skipped and reported rather than having one
  auto-created.
- **Auto-merge is install-time-only, and only for a clean merge.**
  Steady-state `receive` is fast-forward-only and never auto-merges a
  divergence — it always reports it and leaves it for the user, because two
  machines each auto-merging independently would mint rival merge commits
  and re-diverge forever, and `receive` never pushes so it could not recover
  from that on its own. The one softened case is `install`'s initial sync:
  if the two machines' default branches have genuinely diverged, the
  diverged side may attempt a single real merge of `<remote>/<branch>` — but
  only once, only on the diverged side, and only when the default branch is
  what's checked out there (a diverged branch that isn't checked out has no
  worktree to merge into, so it's reported blocked instead, same as always).
  A clean merge is pushed immediately and the other machine then
  fast-forwards onto it; a merge that conflicts is `git merge --abort`ed and
  the repo is reported diverged for the user to merge by hand, exactly as
  any unresolved divergence always has been. Conflicting merges are never
  auto-resolved, at install time or otherwise.
- **Everything depends on the remote being reachable and writable from both
  machines.** git-sync moves no repository data itself: if the remote is down,
  or one machine lacks push access, nothing syncs, and the record shows a
  failed push rather than a silent stall.
- **A repo whose two clones point at different remotes never converges.**
  `install` checks for this and reports it, but a remote changed afterwards
  goes unnoticed until the next `install`; the symptom is a push that succeeds
  and a receive that has nothing to do, forever.
- **Repos outside `base_dir` are invisible to git-sync.** The hook still
  fires in them (`core.hooksPath` is global), but they are skipped
  immediately.
- **Editing `repos` by hand changes only that machine.** `install` keeps the
  two lists in step, but a hand-edit does not propagate. If a repo syncs one
  way but not the other, that is the cause, and the symptom in
  `git-sync report` is "peer has no copy of this repo". Re-run `install` to
  put the two back in agreement.
- **Provisioning copies the binary verbatim**, so both machines must share an
  OS and architecture. A mixed pair (Apple Silicon and x86 Linux, say) needs
  the peer installed by hand with a matching build.
- **A stored SSH password is a stored credential.** It lives in the OS
  keychain, readable by anything running as that user that the keychain lets
  through; on macOS the first background read may raise a "allow access"
  prompt, and re-installing replaces the binary the keychain item trusts, so
  the prompt can return once after an upgrade. A key is still the better
  arrangement; the password path exists for peers where that is not on offer.
- **A password that changes on the peer breaks syncing silently** until
  `install` is re-run: the symptom is `notify` failing with 255 while `ssh`
  by hand still works.
- **`uninstall` is local only.** It does not reach across to the peer; run it
  on each machine you want to clean up.
- **A repo cloned after install stays inert until you re-run `install`** and
  tick it. This is the deliberate cost of opt-in: nothing syncs that you did
  not choose, and the price is one command when you want to adopt something
  new. Re-running also re-syncs the selection to the peer.
