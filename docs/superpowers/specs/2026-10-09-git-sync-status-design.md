# git-sync-status — a menu bar view of this Mac's syncing

Status: implemented.

## Problem

git-sync runs detached and silent: after a commit, the only way to see
whether it went through is to open `git-sync report`. Failures that need a
human (a rejected push, diverged history, a failed `./activate`) are written
to the activity log and then nothing tells you they happened. Deliveries
queued because a machine was off are not visible at all except as files
under `~/.gitsync/pending/`.

## Decision

A small macOS menu bar app, `git-sync-status`, that answers four questions
at a glance: is git-sync working right now, did it succeed, did it hit
problems, and is anything still waiting to be delivered.

- **On demand only.** The user launches it themselves. It is not installed,
  not a login item, and not provisioned to peers. `install` and `uninstall`
  do not touch it.
- **This Mac only.** It shows this machine's own activity log, pending
  queue and in-flight work. It never sshes to a peer.
- **Built here, for here.** A SwiftPM package with no Xcode project. It is
  built only on macOS. The Linux PC and the other Macs are out of scope.
- **git-sync stays the only reader of `~/.gitsync`.** The app never parses
  `activity.jsonl`, `pending/` or `locks/` itself. It runs
  `git-sync status --json --follow` and reads one JSON snapshot per line.
  If those internal formats change later, only Go code has to follow.

## The icon

`git-sync-status/assets/git-sync-status.svg`: the sync arrows ("syncing" by Gregor
Cresnar, the Noun Project) with the git branch glyph (the Noun Project)
inside them. Both are CC BY 3.0, so the README credits them. The canvas
(`viewBox="5 5 90 90"`) has room around the arrows so that no corner clips at
any angle, and the icon is drawn at 22pt to make up for that margin.
`git-sync-status/assets/icon-states-preview.png` shows every state at large
and real size, on light and dark menu bars.

| State | Icon |
|---|---|
| idle, all good | upright and still, template image (follows the light/dark menu bar) |
| syncing | the whole glyph, arrows and git mark together, turns slowly counter-clockwise (one turn every ~2s) |
| problem in any repo | still or turning as above, plus a red dot in the top-right corner that never turns, with a transparent ring cleared around it so it reads as a badge |

Pending deliveries never turn the dot red. A template image is drawn as a
single colour by the system, so the red-dot state is drawn as a non-template
image in the current menu bar text colour, plus the red dot.

The glyph is rasterised once at build time (`rsvg-convert`, 1x and 2x) into
one PNG. The app rotates it per frame and draws the badge itself. The frames
come from a timer that runs only while something is syncing, so an idle app
does no work.

## The window

Clicking the icon opens a transient popover anchored to it (`NSPopover`,
`.transient` behaviour). Clicking anywhere else closes it, and so does Esc.
Clicking the icon again toggles it. There is no Dock icon and no app menu
(`.accessory` activation policy).

The popover holds one table:

```
 Repo              State                 Last sync   Detail
 ────────────────────────────────────────────────────────────────────────
 git-sync          ↻ syncing (push)      now
 agents-configs    ✓ ok                  2 min ago
 top_cpu           ✗ error               1 h ago     push to github failed: rejected (non-fast-forward)
 zsh               ⚠ warn                3 h ago     diverged from github/main, merge by hand
 powerline         ✗ error               1 d ago     ./activate failed (exit 1), see ~/.gitsync/activate/powerline.log
 ────────────────────────────────────────────────────────────────────────
 Pending
 agents-configs    → 192.168.1.3         since 16:49  peer offline, will retry
 git-sync          → 192.168.1.3         since 16:50  peer offline, will retry
```

- **One row per selected repo** in `config.toml`, including a repo with no
  activity yet (`–`, never synced). Sort order: syncing, then problems, then
  the rest by most recent activity.
- **Detail** is the problem's message, worded as `report` words it. The full
  text is in the row's tooltip when it is truncated.
- **Pending** rows are listed below the repos under their own header: one row
  per queued push (`→ remote`) and per queued notify (`→ <peer>`), with the
  time the entry was first queued (the marker file's mtime; `markPending`
  no longer truncates an existing marker, so a retry that fails again keeps
  the original time). The section is hidden
  when the queue is empty.
- **Clicking a row copies the repo's absolute path** (`base_dir/<rel>`) to
  the clipboard, for a repo row or a pending row alike. A brief "Copied"
  flash on the row confirms it.
- A footer line shows the last time a snapshot arrived, and an error line if
  the `git-sync` process itself could not be run.

## A repo's state

The log is read as a set of independent channels per repo, keyed by
`(op, peer)`: `push`, `notify → 192.168.1.1`, `notify → 192.168.1.4`,
`receive`, `activate`. For each channel only the **latest run** counts.

A run is one operation's worth of events. A single receive writes several
events (for example `ok` "stashed", `warn` "diverged", `ok` "restored
stashed changes"), so "latest event" would let the trailing `ok` hide the
`warn`. `activity.Event` therefore has a `Run` id, and `syncRepo` stamps every
event of one receive with the same one. Events with no `Run` (every other op,
and old log lines) are each their own run.

- A repo **has a problem** if the latest run on any of its channels contains a
  `warn` or `error` event. It clears when that same channel next succeeds. A later
  commit that pushes fine clears a rejected push, and the next successful
  `./activate` clears a failed one. There is no time window: a problem stays
  until something fixes it.
- `skip` events are ignored: they never set or clear a problem.
- The repo's **last sync** is the time of its newest `ok` event on any
  channel.
- A repo is **syncing** while any in-flight marker for it exists (see below).

## Offline is not an error

Today a delivery that failed only because a machine was out of reach is
logged as `status: "error"` with a "will retry" message (push.go,
`notifyPeer`). This adds a fifth status:

```go
StatusOffline Status = "offline" // a machine was out of reach; queued in pending/ and retried
```

It is used for exactly the cases that already call `markPending`: a push
that `gitcmd.IsOffline` reads as unreachable, a notify where ssh exits 255,
and a notify where the peer exits `ExitFetchFailed`. Two more cases are
offline too: the receiver's own fetch failure when `gitcmd.IsOffline` says so
(otherwise it stays `error`), and `announce`'s "could not reach the remote at
startup". `IsProblem()` stays
`warn || error`, so `report` stops counting these as problems too. `report`
shows offline events with their own dim marker rather than hiding them.

Old log lines written before this change still say `error`. `status` treats
an `error` event whose message ends in `will retry` as `offline`. That shim
can be deleted once logs older than this change no longer matter.

An offline event neither sets nor clears its channel's problem. It is
reported only through the pending queue, which is the source of truth for
"still owed".

## In-flight markers: knowing that git-sync is working

The log records only how things ended. To show "syncing" while it happens,
the long-running operations drop a marker for as long as they run:

```
~/.gitsync/running/<pid>-<op>-<escaped rel>    JSON: {"repo","op","peer","started"}
```

- A new leaf package, `internal/running`, with
  `Start(rel string, op activity.Op, peer string) (stop func())` and
  `List() []Marker`.
- `push` (including the retries of the pending backlog), `receive` and each
  `./activate` run call `Start` and `defer stop()`.
- `List` drops markers whose pid is no longer alive (`kill(pid, 0)`). A
  crashed process leaves a file behind, but it can never show as syncing
  forever. Those files are removed as they are found.
- One flat file per operation, created and removed atomically: no lock, the
  same reasoning as `pending/`.

## `git-sync status --json [--follow]`

A new hidden subcommand (machine-facing, like `hook`/`push`, absent from
`-h`).

- `--json` prints one snapshot as a single line of JSON and exits.
- `--follow` prints one at start, then polls every 500 ms. It prints a new
  snapshot whenever `activity.jsonl`'s size or mtime, the `pending/` tree,
  the `running/` directory or `config.toml` changed, and once a minute
  regardless, so relative times stay fresh. It exits 0 on stdin EOF or
  SIGTERM, so it never outlives the app. Polling avoids any new dependency
  and costs one `stat` per watched path per tick.
- Without `--json` it prints the same thing as a short human-readable table.
  That is cheap to add, and it is useful for checking the data without the
  app.

Snapshot:

```json
{
  "at": "2026-10-09T17:20:00-06:00",
  "syncing": true,
  "problems": 2,
  "repos": [
    {
      "repo": "top_cpu",
      "path": "/Users/grillermo/c/top_cpu",
      "state": "error",
      "last_sync": "2026-10-09T16:10:00-06:00",
      "running": [{"op": "push", "peer": "", "started": "..."}],
      "problems": [{"ts": "...", "op": "push", "peer": "", "status": "error", "msg": "push to github failed: ..."}]
    }
  ],
  "pending": [
    {"kind": "notify", "repo": "agents-configs", "path": "...", "peer": "192.168.1.3", "since": "..."}
  ]
}
```

`state` is one of `syncing`, `error`, `warn`, `ok`, `never`. It is the
highest-ranked state that applies, so a repo that is both syncing and in
error reports `syncing` and still lists its problems. The aggregation is a
pure function over `[]activity.Event` + markers + pending entries + config,
like `report/aggregate.go`, and is tested the same way, with no I/O.

`pending.go` gains an exported `ListAllPending()` returning every entry with
its kind, rel, host and mtime. Today the listing helpers are internal to the
retry path.

## The app

Everything the app needs lives in `git-sync-status/` at the repo root. Its
only link to the Go code is the `git-sync status` command it runs.

```
git-sync-status/
  build                         builds, installs into /Applications, relaunches
  Package.swift                 two targets (Core library, executable), macOS 15+, Swift 5 language mode
  assets/
    git-sync-status.svg         the merged icon
    icon-states-preview.png     every icon state, light and dark
    noun_syncing_3560918.svg    source icons, kept for attribution
    noun_git_4941290.svg
  Sources/GitSyncStatusCore/    library, no AppKit, unit-tested
    Snapshot.swift              Codable mirror of the JSON above
    Rows.swift                  row mapping, relative times, IconState
  Sources/GitSyncStatus/        the AppKit/SwiftUI executable
    main.swift                  NSApplication, .accessory policy
    AppDelegate.swift           wires the feed to the status item
    StatusFeed.swift            runs the git-sync process, decodes snapshots, restarts it
    IconRenderer.swift          rotates the glyph, draws the red badge
    StatusItemController.swift  NSStatusItem, icon frames, popover toggle, Esc
    StatusTable.swift           hand-laid SwiftUI grid (LazyVStack of fixed-width
                                columns, not SwiftUI Table) inside the popover
  Tests/GitSyncStatusCoreTests/ swift-testing tests for the Core library
  git-sync-status.app           build output (gitignored), copied to /Applications
```

- The app runs `~/.gitsync/bin/git-sync status --json --follow`, the
  installed copy the hooks use, not `bin/` in this repo. If that process
  exits (for example `./activate` replaced the binary), the app starts it
  again after 2 s and shows "reconnecting" in the footer meanwhile.
- `git-sync-status/build` is the app's own build script, separate
  from the repo's `./build` and from `make`. It renders the icon
  (22pt PNG plus @2x into `Contents/Resources`, loaded with `Bundle.main`;
  there are no SwiftPM resources), runs `swift test` (a failing test never
  replaces the running app), then `swift build -c release`, assembles `git-sync-status/git-sync-status.app` with an `Info.plist`
  setting `LSUIElement`, quits the running copy, replaces
  `/Applications/git-sync-status.app` and starts it. It runs after every
  successful change to the app or to `git-sync status` (see AGENTS.md).
  `make build` and `make check` never touch the app, so they keep working
  on Linux.
- No code signing beyond the ad-hoc signature `swift build` applies. It runs
  only on the machine that built it.

## Testing

- Go: table tests for the status aggregation (channels, clearing, offline vs
  error, the legacy "will retry" shim, sort order) and for `running` (stale
  pid reclaimed). There is also a sandboxed test that `push`, `receive` and
  `activate` leave no marker behind, and one that `status --json` matches a
  fixture. The existing tests that assert `error` for offline cases change to
  `offline`.
- Swift: decoding a recorded snapshot fixture, and the row ordering. Anything
  more is checked by running the app.

## Out of scope

- Mesh-wide status (peers' logs), notifications, a login item, a Linux tray
  equivalent.
- Acting from the window (unlock, retry now, open report). The only action
  is copying a repo's path.
