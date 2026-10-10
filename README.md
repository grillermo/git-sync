# git-sync

git-sync keeps a chosen set of git repos in sync across a mesh of two or more
machines. Commit in a selected repo on any machine, and the commit shows up
on every other machine with no manual step.

## How it works

The commits travel through each repo's own git remote, which every machine
pushes to and pulls from. SSH carries one thing only: a "there is something
to pull" nudge, sent to every other machine in the mesh in parallel. No
repository data ever moves machine to machine over SSH.

A commit fires a global post-commit hook, which pushes the current branch to
the repo's shared remote in the background and then SSHes every other
machine in the mesh to run its own `git-sync receive`, which fetches that
same remote and fast-forwards onto it.

While a machine is receiving a repo, that repo refuses local commits and
pushes on that machine (blocking `pre-commit`/`pre-push` hooks) until the
receive finishes, so a commit can never be made - and broadcast out - in the
middle of a sync. A crashed or killed receive cannot wedge a repo forever: a
stale lock lets go on its own after a few minutes, and `git-sync unlock` (see
below) clears one immediately by hand.

Which remote is "shared" decides whether a given repo can sync at all: a
remote named `github` if the repo has one, otherwise `origin`, otherwise its
single remote if it has exactly one. A repo with none of those has nothing to
sync through. Override the order with `remote_names` in
`~/.gitsync/config.toml`.

## Install

Run this once, on any one machine, naming every other machine in the mesh:

```bash
go build -o git-sync ./cmd/git-sync
./git-sync install ~/code --peer you@other-machine.local --peer you@a-third-machine
```

`--peer user@host[:base_dir]` is repeatable - one per other machine in the
mesh. Add `:base_dir` when that machine lays its repos out under a different
path than this one. `--peer-host`/`--peer-user` still work as a single-peer
shorthand for the two-machine case.

Or name none: on a terminal with no peer configured yet, `install` scans the
local network for ssh hosts - a Bonjour browse for `_ssh._tcp`/`_sftp-ssh._tcp`
plus a port-22 sweep of this machine's /24, for about six seconds - and lists
what it finds in a checkbox picker as the machines turn up. Tick the ones to
sync with and give the username to use on them (defaults to yours here).
`--discover` runs the same scan even when peers are already configured, to add
more. Discovery only suggests: every machine picked still goes through the
key-only connect check below. A Mac advertises itself over Bonjour when
Remote Login is on; a Linux box does when avahi publishes its ssh service,
and is otherwise found by the port sweep, by IP.

The wizard runs in four stages: **connect, pick, verify, install**.

- **Connect** checks that this machine can reach every peer over key-only
  ssh, then checks every *other* pair in the mesh too - not just this
  machine to each peer, but peer to peer as well, since a missing key
  between two peers would otherwise only show up later as a failing notify
  nobody is watching. A pair that cannot connect is a warning, printed with
  what is actually wrong (host key never seen, host key changed, key not
  accepted, sshd off, name does not resolve) and the exact command that fixes
  that pair - run on the right machine, e.g. `ssh -t you@b 'ssh-copy-id you@c'`
  for a peer-to-peer pair. On a terminal, install offers to run them for you,
  then checks again; replacing a *changed* host key is asked separately and
  needs a typed `yes`. The rest of the mesh is still set up either way.
- **Pick** opens the repo checkbox picker; tick what you want synced. Each row
  names the remote that repo would sync through.
- **Verify** asks every reachable peer which of those repos it actually has,
  and whether its clone points at the same remote, and lists everything that
  does not line up. The picker can only see this machine, so a one-sided or
  differently-remoted repo would otherwise never sync and never say why.
- **Install** writes the local hooks and config, then sets up every peer over
  SSH - binary, config (with that peer's own view of the mesh), hooks and
  all - so nothing is typed on any of them.

In the picker, type to filter, `enter` ticks a repo and `tab` moves to the
Save and Cancel buttons (`enter` runs the focused one). `esc` clears the
filter, then quits; `ctrl+c` quits at once. Press `q` at the verify screen
to quit. Either way nothing is changed on any machine.

Flags:

| Flag | Effect |
|---|---|
| `--peer user@host[:base_dir]` | another machine in the mesh (repeatable) |
| `--discover` | scan the local network for more machines even when peers are already configured |
| `--all` | sync every repo found; skip the picker |
| `--pick` | reopen the repo picker instead of reusing the selection saved by an unfinished install |
| `--repos a,b,c` | sync exactly these repos; skip the picker (required when there is no terminal) |
| `--no-peer` | set up this machine only |
| `--no-initial-sync` | skip levelling the selected repos with their remotes |
| `--self-host` | this machine's hostname, if a peer cannot reach it by its system hostname |
| `--self-user` | the account peers should SSH back into |
| `--peer-base-dir` | base_dir override for `--peer-host`/`--peer-user` (use `user@host:base_dir` with `--peer` instead) |

Until a run pairs every machine, the repos you ticked are saved in the temp
dir (`git-sync-install-repos-<uid>.json`) and reused without asking, so you
can re-run `install` while fixing ssh without re-picking them; it is removed
once every machine is reachable and every pair connects.

Re-running `install` adds to the existing mesh rather than replacing it - but
only on the machine you run it on: it merges its own existing peer list with
whatever new `--peer` flags you pass. Each *peer* it then provisions gets its
config.toml **replaced** with that merged list, not merged with whatever
peers that peer's config already had - so re-provisioning a peer from a new
machine can silently drop a peer it already knew about. `install` prints a
warning naming this when it re-provisions an already-configured peer.

## Prerequisites

- SSH working, by key, from every machine in the mesh to every other. There
  is no password fallback: git-sync is key-only, because nothing in the sync
  path has a terminal that could answer a prompt from a detached hook.
  `install`'s connect stage checks every pair and tells you exactly which
  ones need `ssh-copy-id`.
- Peers on macOS or Linux, arm64 or amd64. `./build` produces
  `bin/git-sync-<os>-<arch>` for all four (or `./build linux/amd64` for one),
  and `install` sends each peer the build matching its platform. Run it from
  `bin/git-sync` so it can find the other builds next to it; a missing build
  is reported with the exact `./build` command to make it.
- The same repo cloned on every machine at the same path *relative to
  `base_dir`* **and from the same remote**. Two clones of different
  repositories at the same path never converge, which is why install
  compares the remote URLs and says so.
- Push access to that remote from every machine.
- `base_dir` need not be the same absolute path on every machine.

## The commands

- `git-sync install <base_dir>` - see above.
- `git-sync install` (no `base_dir`, run inside a repo) - add just that repo,
  no scan and no repo picker; the machine picker always opens so you choose
  where it syncs. Keeps an existing install's `base_dir` (the repo must be
  under it) and its other repos; with nothing installed yet it asks for a
  `base_dir`, defaulting to the repo's parent.
- `git-sync report [flags]` - browse sync activity, grouped by repo.
  - `--since 24h` - only show activity newer than this
  - `--repo <substr>` - only show repos whose path contains this
  - `--errors` - only show warnings and errors
  - `--plain` - force static output even on a terminal
  - Interactive keys: `↑`/`↓` to select a repo, `e` to toggle problems-only,
    `q` to quit. Piping the output (or `--plain`) produces static, greppable
    text instead.
- `git-sync unlock [<repo>]` - clear a stuck sync lock by hand (default: the
  repo in the current directory). Needed only if a receive died mid-sync
  (killed process, machine went to sleep) and you don't want to wait out the
  stale-lock timeout.
- `git-sync activate [<repo>]` - run a repo's `./activate` now (default: the
  repo in the current directory), in the foreground. See below. Ctrl-C stops
  the script and releases the repo.
- `git-sync uninstall [--purge] [--local]` - remove git-sync from every
  machine in the mesh (`--local` limits it to this machine only; `--purge`
  also deletes config and activity history on whichever machines it touches).

## Menu bar app (macOS)

`git-sync-status/` holds a small menu bar app that shows this Mac's syncing at
a glance. The icon turns while git-sync is working and gets a red dot when any
repo has a problem (a rejected push, diverged history, a failed `./activate`).
Click it for a table with one row per synced repo (state, last sync, detail)
and the deliveries still queued for machines that are off. Clicking a row
copies the repo's path. It reads only `git-sync status --json --follow`.

Install it with `git-sync-status/build` (needs Xcode's Swift toolchain and
`brew install librsvg`). It tests, builds, copies the app into
`/Applications` and starts it. It runs on demand only: it is not a login item
and `git-sync install` does not set it up.

The app runs the installed `~/.gitsync/bin/git-sync`, so that copy needs the
`status` command first: run `make build` and re-install (or let `./activate`
do it) before the app can show anything.

Icon credits: "syncing" by Gregor Cresnar and "git", both from the Noun
Project, CC BY 3.0.

## Making synced code live: `./activate`

When a sync moves a repo forward on a machine and that repo has an
executable `./activate` at its root, git-sync runs it there afterwards - to
rebuild a binary, restart a service, whatever "deploy" means for the repo.
Only machines that *receive* the change run it, never the one that committed.

The script must:

- be executable and live at the repo root;
- be idempotent, since it can run when nothing relevant changed;
- not expect a terminal (stdin is not a tty) and set its own `PATH`, because
  it runs from a background process, not your shell;
- read `GITSYNC_OLD_REV` and `GITSYNC_NEW_REV` to see what changed
  (`git diff "$GITSYNC_OLD_REV" "$GITSYNC_NEW_REV" -- some/path`). Several
  quick commits are merged into one run, with the oldest `OLD_REV`;
- exit 0 on success. Anything else is recorded as an error in
  `git-sync report`.

Runs are queued and go one repo at a time per machine. While a repo's
`./activate` runs, commits and pushes in it are refused, with a message saying
so, just as during a receive. Output goes to `~/.gitsync/activate/<repo>.log`.
A run is stopped after 15 minutes. A failed run is not retried; the next
sync that changes the repo queues it again.

`git-sync activate [<repo>]` runs it by hand. A manual run passes git's
empty-tree hash as `GITSYNC_OLD_REV`, so a diff against it sees every file as
changed and the script does the full job.

For a service that only needs restarting, two lines are enough:

```sh
#!/bin/sh
exec ./serve
```

## Repos that exist on only one machine

Install warns about these up front (`missing`/`not-a-repo`, listed per repo,
per peer). At runtime nothing happens, by design: the commit pushes
normally, a machine without the repo records `not on this machine`, the
pusher records `no copy of this repo`. No error, no retry, no auto-clone. To
start syncing one, clone it by hand on the other machine under its
`base_dir` at the same relative path; the next commit picks it up.

## Troubleshooting

Start with `git-sync report --errors`. `~/.gitsync/debug.log` has the raw git
and ssh output. Nothing can prompt you, so these are the only record.

A notify failing with exit 255 while `ssh <peer>` works by hand from the same
account usually means a key that worked when installed no longer does (an
`authorized_keys` rewrite, a rotated key). Fix ssh directly and the next
commit will pick it back up - there is nothing else for git-sync to retry.

If a repo refuses every commit with "receiving changes from ...", either
wait for that receive to finish or run `git-sync unlock <repo>` if it looks
stuck.

## Repos with no remote

They cannot sync, because there is nothing to sync through. The picker marks
them `no remote - cannot sync`, and if one is selected anyway every commit
records a warning naming it. `git remote add` on every machine is the fix.

## Known limitations

- `core.hooksPath` is global and exclusive. It replaces, rather than chains
  with, repo-local hooks such as Husky or the `pre-commit` framework - this
  includes the *name* `pre-commit`: git-sync installs its own hook of that
  name, which is a different thing from that tool's hook. If a repo needs
  its own hooks alongside git-sync, they must be invoked by hand from the
  shim in `~/.gitsync/hooks/`.
- The receiver syncs whatever branch *it* has checked out, against that
  branch on the shared remote - not necessarily the branch that was just
  pushed. Different branches on different machines means there is nothing to
  fast-forward, which is normal and not an error.
- The remote is resolved by name, not from the branch's upstream. A branch
  tracking `origin/main` in a repo that also has a `github` remote syncs
  through `github`; `git-sync report` names the remote it used on every push
  and receive.
- Every machine in the mesh must be reachable over key-only ssh from every
  other. `install`'s connect stage warns about any pair that cannot connect
  but sets up the rest of the mesh regardless; that pair simply will not
  sync until the key is added.

## Uninstalling

`git-sync uninstall` removes git-sync from every machine in the mesh, keeping
each one's config and activity history. `git-sync uninstall --purge` also
removes those. `--local` limits either to just the machine you run it on.
