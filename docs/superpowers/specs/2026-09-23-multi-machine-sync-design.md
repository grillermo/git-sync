# git-sync: Multi-Machine Sync — Design

Supersedes the two-machine parts of
`2026-08-22-git-sync-design.md`. Everything that document says about the
remote being the transport, the repo allowlist, the activity log and the
receive algorithm still holds; this spec changes who talks to whom, adds a
receiver state, and drops SSH password support.

## Goal

Let a third (and fourth, and fifth) machine join the sync. Every machine
broadcasts its own commits to every other machine, and no machine applies
incoming changes and produces its own at the same time.

## Requirements

1. Every machine is a broadcaster: a commit in a selected repo is pushed to
   the shared remote, and every other machine is told to pull it.
2. A machine is a broadcaster or a receiver for a given repo, never both at
   once.
3. While a machine is receiving a repo, commits made in that repo do not
   broadcast.
4. The receiver state is a lock, held for exactly as long as it takes to
   process one incoming notification.
5. While a machine is receiving a repo, `git commit` and `git push` in that
   repo are refused with a message explaining why.

The user does not edit the same repo on two machines at once. That is an
assumption, not something this design enforces; where it is violated the
result is the existing "diverged, merge by hand" report.

## Decisions

| Question | Decision |
|---|---|
| Lock scope | Per repo. A receive of `notes` does not block commits in `work/api`. |
| Broadcast trigger | `post-commit` only. Git has no post-push hook; a manual `git push` is carried by the next commit. |
| Broadcaster state | None. Only the receiver holds a lock. |
| Stale locks | Reclaimed automatically after 5 minutes; `git-sync unlock <repo>` clears one immediately. |
| Joining machines | Re-run `install` with every peer named; it builds a full mesh. |
| Addressing | One host/user per machine, assumed reachable under the same name from every other machine. |
| Authentication | SSH keys only. The password, keychain, askpass and savepass machinery is deleted. |
| Key verification | `install` checks every ordered pair and prints the failures. It warns; it does not abort. |
| Fan-out | Peers are notified in parallel, one `notify` event each. |
| Initial sync | Push from the first machine that is ahead, fast-forward those behind, report the rest. |
| Uninstall | Removes git-sync from every machine in the mesh. |

## Configuration

`config.toml` gains a list of peers and loses `peer_host`/`peer_user`:

```toml
base_dir = "/Users/guillermo/code"

[[peers]]
host     = "laptop.local"
user     = "guillermo"
base_dir = "/Users/guillermo/code"

[[peers]]
host     = "desktop.local"
user     = "guillermo"
base_dir = "/home/guillermo/code"

repos = ["notes", "work/api"]
remote_names = ["github", "origin"]
```

Each machine's list holds every *other* machine; a machine never lists
itself. `base_dir` on a peer entry is that machine's sync root, which is how
provisioning already handles a peer whose home directory differs.

`Load` migrates the old form: a file with `peer_host`/`peer_user` and no
`[[peers]]` is read as a single-peer list, so an existing two-machine install
keeps working untouched until the next `install` rewrites it. `Save` always
writes the new form.

`Config` grows `Peers []Peer`; the `PeerHost`/`PeerUser` fields survive only
as migration inputs and are not written back.

## The receiver lock

The existing per-repo lock in `internal/lock` becomes the receiver state.
No second lock is introduced.

- `locks/<rel>.lock/` is still the mkdir-atomic directory.
- It now contains an `owner` file: the notifying machine's host, the start
  time, and the receiving process's pid.
- `receive` refreshes the `owner` file's timestamp every 60 seconds while it
  works, so a slow fetch on a large repo is never mistaken for a dead holder.
- `StaleAfter` stays 5 minutes, measured against that timestamp.
- New: `lock.Held(rel) (Owner, bool)` — a read-only check for the hooks, true
  only for a lock that exists and is not stale.
- New: `lock.Break(rel)` — what `git-sync unlock` calls.

## Hooks

Three shims in `~/.gitsync/hooks/`, all execing the same binary:

```
hooks/post-commit   → git-sync hook post-commit
hooks/pre-commit    → git-sync hook pre-commit
hooks/pre-push      → git-sync hook pre-push
```

`core.hooksPath` is already global and exclusive, so adding hook types
changes nothing about repo-local hooks — they were already bypassed.

**`pre-commit` / `pre-push`** resolve the repo the same way `post-commit`
does. If it is selected and its lock is held, they print

```
git-sync: notes is receiving changes from laptop.local (started 8s ago).
Wait a moment and try again. If this is stuck: git-sync unlock notes
```

and exit 1. In every other case they exit 0 — including when the config is
missing, the repo is outside `base_dir` or unselected, `GITSYNC_INTERNAL=1`
is set, or git-sync itself errors. **The hooks fail open.** A broken git-sync
must never stop the user committing; only a live lock blocks.

**`post-commit`** gains one check before spawning the push: if the repo's
lock is held, it logs a `warn` ("committed while receiving, not broadcast")
and stops. This is what makes requirement 3 true even when the commit reached
git through `--no-verify`, which skips `pre-commit` but not `post-commit`.

**`GITSYNC_INTERNAL=1`** is set on every git invocation git-sync makes
itself, so its own pushes (background push, initial sync) cannot be refused
by its own `pre-push` hook. `receive` needs no exemption: fetch, stash and
`merge --ff-only` run neither hook.

## Behavior

### `install <base_dir>`

Flags: `--peer user@host[:base_dir]`, repeatable. Peers named on the command
line are merged with those already in `config.toml`; with no `--peer` and no
existing config, install prompts as it does today. `--no-peer`,
`--no-initial-sync`, `--self-host`, `--self-user` keep their meanings;
`--peer-host`/`--peer-user` remain as aliases for a single `--peer`.

Stages, in order:

1. **Reachability.** `ssh -o BatchMode=yes <peer> true` for each peer. A peer
   that fails is reported and skipped for the rest of the run; install
   continues with the others.
2. **Pairwise keys.** On each reachable peer, run the same check against
   every *other* machine. Print a table of the ordered pairs that fail, with
   the `ssh-copy-id` command that fixes each. Warn only — the mesh is still
   installed, and those pairs simply will not sync until the key is added.
3. **Repo check.** The existing "does the peer have this repo, and does it
   point at the same remote" check, run against every peer. Mismatches are
   reported per machine.
4. **Provision.** Copy the binary, write the three hook shims, set
   `core.hooksPath`, and write each machine's `config.toml`: the full machine
   list minus that machine, plus this one.
5. **Initial sync.** Across all machines (below).

### Initial sync

For each selected repo, measure every machine against the shared remote:
ahead, behind, equal, diverged, or not present / different branch.

- Push from the first machine that is purely ahead — this machine first, then
  peers in config order.
- Fast-forward every machine that is then purely behind, stashing around the
  merge exactly as `receive` does.
- Report everything else: other machines that were also ahead, genuinely
  diverged history, and machines on a different branch. Nothing is merged
  automatically and nothing is force-pushed.

### `hook post-commit`

Unchanged except for the receiver check described above.

### `push <repo>`

After the `git push`, notify every peer **in parallel**, each with its own
ssh:

```
ssh -o BatchMode=yes user@host '~/.gitsync/bin/git-sync receive <rel> --from <this-host>'
```

Each peer produces its own `notify` event, with `peer` set. One unreachable
peer does not delay or affect the others. There is still no retry queue: a
peer that missed a notification catches up on the next commit.

### `receive <repo> [--from <host>]`

Unchanged in what it does to the repository. Changes:

- Acquires the lock before doing anything to the repo, records `--from` in
  the `owner` file, refreshes it while working, and releases it at the end.
- `--from` is used only for the lock's owner record and the blocking message.
  It is untrusted input, like `<repo>`, and is sanitised before display.

### `unlock [<repo>|<path>]`

New. Defaults to the repo containing the current directory. Prints who held
the lock and when it started, removes it, and logs the removal. Exits 0 when
there was no lock.

### `uninstall`

Now mesh-wide. Runs `git-sync uninstall --local` on every peer over ssh, then
uninstalls this machine. `--local` is the current behavior (hook shims,
`core.hooksPath`, binary) and is what the peers run. Machines that cannot be
reached are listed at the end, with the command to run on them by hand.

## Logging

No new `op` values. `notify` events already carry `peer`, which now
distinguishes one fan-out from another. New messages:

- `hook` / `warn` — "committed while receiving from `<host>`, not broadcast".
- `receive` / `skip` — unchanged for lock contention.

## Exit codes

Unchanged: 0 success, 1 failure, 2 usage, 3 `receive` only, "no copy of that
repo here". The blocking hooks exit 1, which is what git requires to abort a
commit or push.

## Removed

- `internal/secret` and the `GITSYNC_SECRET_BACKEND` escape hatch.
- `askpass` / `savepass` commands and the `~/.gitsync/askpass` shim.
- The password prompt and password-detection paths in
  `internal/setup/sshauth.go`; what remains is a plain reachability check.
- `sshx` always uses `BatchMode=yes`.

Uninstall removes a leftover `askpass` shim if it finds one, so upgrading
from the password era leaves nothing behind.

## Testing

`testutil.Sandbox` is unchanged. The end-to-end harness generalises from two
machines to N:

- `peerMachine` becomes `machine(name)`, callable any number of times, each
  with its own `HOME`/`GITSYNC_HOME`.
- `installLoopbackSSH` dispatches on the ssh target: a shell `case` mapping
  each host to that machine's `HOME`/`GITSYNC_HOME` before running the
  remaining command. This also covers peer-to-peer ssh during the pairwise
  key check, since the stub is on every fake machine's `PATH`.

Unit coverage:

- **config** — old two-field file migrates to a one-entry peer list; a
  multi-peer file round-trips; each machine's written config excludes itself.
- **lock** — `owner` records host and start time; `Held` is false once stale
  and true while fresh; a refreshed lock outlives `StaleAfter`; `Break`
  removes it and returns the previous owner.
- **syncer/hook** — `pre-commit` blocks with the peer's name while held;
  allows when stale, unselected, outside `base_dir`, with no config, or under
  `GITSYNC_INTERNAL=1`. `post-commit` does not broadcast while the lock is
  held and logs a `warn`.
- **syncer/push** — three peers all notified; one unreachable peer still
  leaves the other two `ok`; the ssh calls overlap in time.
- **setup** — the pairwise matrix renders failures and install continues;
  each peer's config excludes that peer and includes this machine; uninstall
  reaches every peer and lists the unreachable ones.
- **setup/initialsync** — one machine ahead: it pushes, the others
  fast-forward. Two ahead: the first pushes, the second is reported.

End-to-end, with three machines and a real binary: a commit on A reaches B
and C; a commit attempt on B while B holds a receiver lock is refused, while
C is unaffected; install then uninstall leaves no `core.hooksPath` anywhere.

## Known limitations

Everything in the original spec's list still applies, minus the two password
entries and minus "uninstall is local only". Added:

- **The mesh assumes one address per machine.** A machine reachable under
  different names from different networks needs those names settled in ssh
  config, not in git-sync.
- **`--no-verify` still commits during a receive.** The commit is allowed and
  simply not broadcast; the next ordinary commit carries it.
- **A missing key between two peers is invisible after install.** It is
  checked at install time and shows up afterwards only as a failing `notify`
  in `report`.
- **The receiver lock does not stop a commit already in flight.** A commit
  that passed `pre-commit` microseconds before a receive takes the lock still
  lands; the result is the existing diverged report, not corruption.
- **Provisioning still copies the binary verbatim**, so every machine in the
  mesh must share an OS and architecture.
