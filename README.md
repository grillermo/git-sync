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

The wizard runs in four stages: **connect, pick, verify, install**.

- **Connect** checks that this machine can reach every peer over key-only
  ssh, then checks every *other* pair in the mesh too - not just this
  machine to each peer, but peer to peer as well, since a missing key
  between two peers would otherwise only show up later as a failing notify
  nobody is watching. A pair that cannot connect is a warning, printed with
  the `ssh-copy-id` command to fix it; the rest of the mesh is still set up.
- **Pick** opens the repo checkbox picker; tick what you want synced. Each row
  names the remote that repo would sync through.
- **Verify** asks every reachable peer which of those repos it actually has,
  and whether its clone points at the same remote, and lists everything that
  does not line up. The picker can only see this machine, so a one-sided or
  differently-remoted repo would otherwise never sync and never say why.
- **Install** writes the local hooks and config, then sets up every peer over
  SSH - binary, config (with that peer's own view of the mesh), hooks and
  all - so nothing is typed on any of them.

Press `q` at either the picker or the verify screen to quit with nothing
changed on any machine.

Flags:

| Flag | Effect |
|---|---|
| `--peer user@host[:base_dir]` | another machine in the mesh (repeatable) |
| `--all` | sync every repo found; skip the picker |
| `--repos a,b,c` | sync exactly these repos; skip the picker (required when there is no terminal) |
| `--no-peer` | set up this machine only |
| `--no-initial-sync` | skip levelling the selected repos with their remotes |
| `--self-host` | this machine's hostname, if a peer cannot reach it by its system hostname |
| `--self-user` | the account peers should SSH back into |
| `--peer-base-dir` | base_dir override for `--peer-host`/`--peer-user` (use `user@host:base_dir` with `--peer` instead) |

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
- All machines on the same OS and architecture - the binary is copied
  verbatim.
- The same repo cloned on every machine at the same path *relative to
  `base_dir`* **and from the same remote**. Two clones of different
  repositories at the same path never converge, which is why install
  compares the remote URLs and says so.
- Push access to that remote from every machine.
- `base_dir` need not be the same absolute path on every machine.

## The commands

- `git-sync install <base_dir>` - see above.
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
- `git-sync uninstall [--purge] [--local]` - remove git-sync from every
  machine in the mesh (`--local` limits it to this machine only; `--purge`
  also deletes config and activity history on whichever machines it touches).

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
