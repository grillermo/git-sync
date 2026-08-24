# git-sync

git-sync keeps a chosen set of git repos in sync between two machines. Commit
in a selected repo on one machine, and the commit shows up on the other with
no manual step.

## How it works

The commits travel through the repo's own git remote, which both machines
push to and pull from. SSH carries one thing only: a "there is something to
pull" nudge. No repository data ever moves machine to machine.

A commit fires a global post-commit hook, which pushes the current branch to
the repo's shared remote in the background and then SSHes the peer to run its
own `git-sync receive`, which fetches that same remote and fast-forwards onto
it.

Which remote is "shared" decides whether a given repo can sync at all: a
remote named `github` if the repo has one, otherwise `origin`, otherwise its
single remote if it has exactly one. A repo with none of those has nothing to
sync through. Override the order with `remote_names` in
`~/.gitsync/config.toml`.

## Install

Run this once, on either machine:

```bash
go build -o git-sync ./cmd/git-sync
./git-sync install ~/code --peer-host other-machine.local --peer-user you
```

The wizard runs in four stages: **connect, pick, verify, install**.

- **Connect** comes first. If the peer wants a password rather than a key, it
  is asked for here, once, checked against the peer and saved to your
  keychain before anything else happens.
- **Pick** opens the repo checkbox picker; tick what you want synced. Each row
  names the remote that repo would sync through.
- **Verify** asks the peer which of those repos it actually has, and whether
  its clone points at the same remote, and lists everything that does not
  line up. The picker can only see this machine, so a one-sided or
  differently-remoted repo would otherwise never sync and never say why.
- **Install** writes the local hook and config, then sets up the peer over
  SSH - binary, config, hook and all - so nothing is typed on it.

Press `q` at either the picker or the verify screen to quit with nothing
changed on either machine.

Flags:

| Flag | Effect |
|---|---|
| `--all` | sync every repo found; skip the picker |
| `--repos a,b,c` | sync exactly these repos; skip the picker (required when there is no terminal) |
| `--no-peer` | set up this machine only |
| `--self-host` | this machine's hostname, if the peer cannot reach it by its system hostname |
| `--self-user` | the account the peer should SSH back into |
| `--peer-base-dir` | the peer's sync root, if the two machines lay their repos out differently |

## Prerequisites

- SSH working from the installing machine to the peer. A key is the smooth
  path. If the peer asks for a password instead, `install` asks you for it
  once, checks it against the peer, and saves it to your keychain (macOS
  Keychain, or libsecret on Linux). Every sync after that is silent - nothing
  can prompt you from a post-commit hook, which is exactly why the password
  cannot be left to be typed later. It is never written to `config.toml`,
  `debug.log` or a command line. `git-sync uninstall --purge` forgets it.
- Both machines on the same OS and architecture - the binary is copied
  verbatim.
- The same repo cloned on both at the same path *relative to `base_dir`*
  **and from the same remote**. Two clones of different repositories at the
  same path never converge, which is why install compares the remote URLs
  and says so.
- Push access to that remote from both machines.
- `base_dir` need not be the same absolute path on both machines.

## The three commands

- `git-sync install <base_dir>` - see above.
- `git-sync report [flags]` - browse sync activity, grouped by repo.
  - `--since 24h` - only show activity newer than this
  - `--repo <substr>` - only show repos whose path contains this
  - `--errors` - only show warnings and errors
  - `--plain` - force static output even on a terminal
  - Interactive keys: `↑`/`↓` to select a repo, `e` to toggle problems-only,
    `q` to quit. Piping the output (or `--plain`) produces static, greppable
    text instead.
- `git-sync uninstall [--purge]` - remove the hook (`--purge` also deletes
  config and activity history).

## Repos that exist on only one machine

Install warns about these up front (`missing`/`not-a-repo`, listed per repo).
At runtime nothing happens, by design: the commit pushes normally, the peer
records `not on this machine`, the pusher records `no copy of this repo`. No
error, no retry, no auto-clone. To start syncing one, clone it by hand on the
other machine under its `base_dir` at the same relative path; the next commit
picks it up.

## Troubleshooting

Start with `git-sync report --errors`. `~/.gitsync/debug.log` has the raw git
and ssh output. Nothing can prompt you, so these are the only record.

A notify failing with exit 255 while `ssh peer` works by hand means the
stored password stopped working or the askpass helper is broken - run
`~/.gitsync/askpass` directly to see whether it still prints anything, and
re-run `install` to re-enter the password.

## Repos with no remote

They cannot sync, because there is nothing to sync through. The picker marks
them `no remote - cannot sync`, and if one is selected anyway every commit
records a warning naming it. `git remote add` on both machines is the fix.

## Known limitations

- `core.hooksPath` is global and exclusive. It replaces, rather than chains
  with, repo-local hooks such as Husky or `pre-commit`. If a repo needs its
  own hooks alongside git-sync, they must be invoked by hand from the shim.
- The receiver syncs whatever branch *it* has checked out, against that
  branch on the shared remote - not necessarily the branch that was just
  pushed. Different branches on the two machines means there is nothing to
  fast-forward, which is normal and not an error.
- A stored password lives in the OS keychain, and the first time a
  background sync reads it macOS may ask you to allow access - choose
  "Always Allow". Re-installing replaces the binary the keychain item
  trusts, so that prompt can come back once after an upgrade.
- The remote is resolved by name, not from the branch's upstream. A branch
  tracking `origin/main` in a repo that also has a `github` remote syncs
  through `github`; `git-sync report` names the remote it used on every push
  and receive.

## Uninstalling

`git-sync uninstall` keeps your config and history. `git-sync uninstall
--purge` removes everything, including the saved peer password.
