# ./activate (per-repo scripts) Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every in-scope repo under `~/c` an `./activate` that git-sync can run unattended, and bring the three foreground services' `./serve` up to the restart-in-place contract.

**Architecture:** Developer tools get an `activate` that rebuilds or reinstalls, and skips when the rev range touched nothing relevant. The five personal services get a two-line `activate` that delegates to `./serve` when the service's tmux session exists on this machine. The three foreground `serve` scripts are rewritten on a shared helper, `~/c/server/serve-in-place.sh`, which generalizes `file_server/serve`. Open shells get a one-line notice from a `precmd` hook in `~/c/zsh/init`.

**Tech Stack:** POSIX sh / bash, zsh, tmux, shellcheck.

**Spec:** `~/c/git-sync/docs/superpowers/specs/2026-10-09-activate-design.md`

**Prerequisite:** `docs/superpowers/plans/2026-10-09-activate-git-sync.md` must be done, and its binary rolled out to every machine (its Task 8 Step 6). From then on, **committing an `activate` file into a synced repo runs it on the other machines straight away.** The first commit is the live test. For the services, it restarts them on the Mac mini. Do the service tasks last, one repo at a time, with the user present.

**Rules for every task:**
- Run `shellcheck` on every sh/bash script before committing (zsh files excepted).
- Each repo is its own git repo under `~/c/<repo>`. Commit there, never in git-sync. git-sync carries the commit to the other machines.
- Every script must work when run by hand too. When `GITSYNC_OLD_REV` is unset or empty, do the full job.
- Never restart a service or touch another machine without asking the user first (`~/.claude/rules/other-machines.md`).

---

## Chunk 1: Developer tools

### Task 1: Go tools: `currentps`, `gworktree`, `disk-space-differ`, `top_cpu`

**Files:** Create `~/c/<repo>/activate` in each of the four repos (identical content).

- [ ] **Step 1: Write `activate`**

```sh
#!/bin/sh
# Run by git-sync after it lands new commits here (see git-sync's README,
# "./activate"): rebuild this machine's binary. Runs with no tty and no login
# shell, so PATH is set here. Safe to run by hand.
set -eu
cd "$(dirname "$0")"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

# Nothing that goes into the binary changed: done.
if [ -n "${GITSYNC_OLD_REV:-}" ] &&
  git diff --quiet "$GITSYNC_OLD_REV" "${GITSYNC_NEW_REV:-HEAD}" -- '*.go' go.mod go.sum build; then
  exit 0
fi
exec ./build
```

For `top_cpu`, add one line above `exec ./build`:

```sh
# The running Mac mini service keeps its old binary until restarted by hand.
```

- [ ] **Step 2: Check and try it**

```bash
for r in currentps gworktree disk-space-differ top_cpu; do
  chmod +x ~/c/$r/activate && shellcheck ~/c/$r/activate && (cd ~/c/$r && ./activate) || echo "FAIL $r"
done
```

Expected: four `built .../bin/<name>-<os>-<arch>` lines and no `FAIL`.

- [ ] **Step 3: Skip check**

```bash
cd ~/c/currentps && GITSYNC_OLD_REV=HEAD GITSYNC_NEW_REV=HEAD ./activate && echo skipped
```

Expected: `skipped` with no build output.

- [ ] **Step 4: Commit each repo**

```bash
for r in currentps gworktree disk-space-differ top_cpu; do
  git -C ~/c/$r add activate && git -C ~/c/$r commit -m "feat: add ./activate for git-sync"
done
```

Then confirm on another machine: `git-sync report`, or `~/.gitsync/activity.jsonl` over ssh. Expect an `activate ok` event per repo.

### Task 2: `git-sync`'s own `activate`

**Files:** Create `~/c/git-sync/activate`.

- [ ] **Step 1: Write it**

```sh
#!/bin/sh
# Run by git-sync after it lands new commits here: rebuild this machine's
# git-sync and install it where the hooks and ssh run it from. The watch
# service sees the replaced binary and restarts itself on it.
set -eu
cd "$(dirname "$0")"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

if [ -n "${GITSYNC_OLD_REV:-}" ] &&
  git diff --quiet "$GITSYNC_OLD_REV" "${GITSYNC_NEW_REV:-HEAD}" -- cmd internal go.mod go.sum build; then
  exit 0
fi

./build "$(go env GOOS)/$(go env GOARCH)"
installed="${GITSYNC_HOME:-$HOME/.gitsync}/bin/git-sync"
# Copy then rename: a hook or ssh mid-exec keeps the old file, never a torn one.
cp bin/git-sync "$installed.new"
mv "$installed.new" "$installed"
echo "installed $(pwd)/bin/git-sync -> $installed"
```

Note: `./build os/arch` only re-points `bin/git-sync` when the target matches this machine. That holds here, since `go env` reports this machine's platform. Under Rosetta on Apple Silicon, `go env GOARCH` can say amd64. If `bin/git-sync` doesn't move, use plain `./build`.

- [ ] **Step 2: Check**

`chmod +x activate && shellcheck activate && ./activate`
Expected: `built ...` then `installed ...`. Run `~/.gitsync/bin/git-sync -h` and check the usage lists `activate`.

- [ ] **Step 3: Commit**

```bash
git add activate && git commit -m "feat: rebuild and reinstall git-sync on sync via ./activate"
```

### Task 3: `powerline`: keep the picked color on rebuild

**Files:** Modify `~/c/powerline/build.sh`; create `~/c/powerline/activate`.

- [ ] **Step 1: Fix `build.sh`.** Replace

```sh
[ -z "${HOSTNAME_COLOR:-}" ] && [ -f .env ] && . ./.env
```

with

```sh
[ -z "${HOSTNAME_COLOR:-}" ] && [ -f .env ] && . ./.env
# ./build records the color it was last built with here; a plain rebuild keeps it.
# shellcheck source=/dev/null
[ -z "${HOSTNAME_COLOR:-}" ] && [ -f color.env ] && . ./color.env
```

- [ ] **Step 2: Verify the fix**

Run `cat color.env; ./build.sh; strings bin/powerline-zsh | grep -c "$(sed -n 's/^HOSTNAME_COLOR=//p' color.env)"`. A cheaper check: open a new shell and confirm the hostname segment still has the color you picked.

- [ ] **Step 3: `activate`**

```sh
#!/bin/sh
# Run by git-sync after it lands new commits here: rebuild the prompt binary
# with this machine's recorded color. Never ./build - that opens a picker.
set -eu
cd "$(dirname "$0")"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

if [ -n "${GITSYNC_OLD_REV:-}" ] &&
  git diff --quiet "$GITSYNC_OLD_REV" "${GITSYNC_NEW_REV:-HEAD}" -- powerline-zsh.go build.sh; then
  exit 0
fi
exec ./build.sh
```

- [ ] **Step 4: Check and commit**

```bash
chmod +x activate && shellcheck activate build.sh && ./activate
git add activate build.sh && git commit -m "feat: add ./activate; rebuilds keep the picked color"
```

### Task 4: `agents-configs`

**Files:** Create `~/c/agents-configs/activate`.

- [ ] **Step 1: Write it**

```sh
#!/bin/sh
# Run by git-sync after it lands new commits here: link new skills and rules
# into ~/.claude, unlink removed ones, register the MCP server and plugins.
# install.sh is idempotent. jq and claude are found via this PATH.
set -eu
cd "$(dirname "$0")"
export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:$PATH"
exec ./install.sh
```

No skip check: `install.sh` is cheap, and edits to existing skills are already live through the symlinks.

- [ ] **Step 2: Check and commit**

```bash
chmod +x activate && shellcheck activate && ./activate
git add activate && git commit -m "feat: add ./activate for git-sync"
```

Expected output ends with `N linked, 0 replaced. 0 removed.`

### Task 5: `zsh`: rebuild `pick`, and tell open shells

**Files:** Create `~/c/zsh/activate`; modify `~/c/zsh/init` (append); modify `~/c/zsh/programs.zsh` (the gworktree comment).

- [ ] **Step 1: `activate`**

```sh
#!/bin/sh
# Run by git-sync after it lands new commits here: rebuild pick. Open shells
# are told by the notice at the end of init; nothing here can reach them.
set -eu
cd "$(dirname "$0")"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

if [ -n "${GITSYNC_OLD_REV:-}" ] &&
  git diff --quiet "$GITSYNC_OLD_REV" "${GITSYNC_NEW_REV:-HEAD}" -- programs/pick; then
  exit 0
fi
exec programs/pick/rebuild
```

- [ ] **Step 2: Notice in `init`.** Append:

```zsh
# git-sync lands commits in the background, so an open shell's config can
# change underneath it. Say so once, before the next prompt. Every HEAD move
# (commit, fast-forward, checkout) appends to .git/logs/HEAD, so its mtime is
# a git-free, per-prompt-cheap signal.
zmodload -F zsh/stat b:zstat
zmodload zsh/datetime
typeset -g _CONFIG_LOADED_AT=$EPOCHSECONDS
_config_changed_notice() {
  local f
  local -a m
  for f in ~/c/zsh/.git/logs/HEAD ~/c/claude-menu/.git/logs/HEAD; do
    [[ -e $f ]] || continue
    zstat -A m +mtime -- $f 2>/dev/null || continue
    if (( m[1] > _CONFIG_LOADED_AT )); then
      print -P "%F{yellow}${f:h:h:h:t} changed since this shell started — run: exec zsh%f"
      precmd_functions=(${precmd_functions:#_config_changed_notice})
      return
    fi
  done
}
precmd_functions+=(_config_changed_notice)
```

- [ ] **Step 3: Fix the stale comment in `programs.zsh`.** Change `Needs ~/c/gworktree/rebuild to have run.` to `Needs ~/c/gworktree/build to have run.`

- [ ] **Step 4: Verify**
  - `shellcheck activate && ./activate` should build pick.
  - Open a new shell, then in another terminal run `git -C ~/c/zsh commit --allow-empty -m "test notice"`. Press Enter in the first shell: the notice should print once, and not again on later prompts. Undo the test commit with `git -C ~/c/zsh reset --soft HEAD~1`, only if it hasn't been synced yet. Otherwise leave it.
- [ ] **Step 5: Commit**

```bash
git add activate init programs.zsh && git commit -m "feat: ./activate rebuilds pick; open shells are told when config changes"
```

### Task 6: Machine-local cleanups (ask before each, on each machine)

Not commits: per-machine state. Do them on this machine, then ask the user before repeating them on the others over ssh.

- [ ] `rm ~/.local/bin/currentps` (a dangling symlink to `~/c/currentps/currentps`; `programs.zsh` already registers the real binary).
- [ ] In `~/.zshrc`, remove the `# git-sync` and `# claude-sessions` PATH blocks. Both are already on PATH via `programs.zsh`, and `~/c/git-sync` has no binary at its root. Show the user the diff first: `~/.zshrc` isn't in a repo.
- [ ] `pipx install --force -e ~/c/claude-swap`, so `cswap` runs the repo instead of the PyPI release. It needs no `activate` after that.

---

## Chunk 2: Personal services

Only the Mac mini runs these, and each `activate` returns without doing anything anywhere else. **Committing an `activate` here restarts that service on the Mac mini a few seconds later.** Do one repo at a time, and tell the user before each commit.

### Task 7: Shared restart-in-place helper

**Files:** Create `~/c/server/serve-in-place.sh`.

- [ ] **Step 1: Write it.** This is `file_server/serve`'s machinery, generalized. `file_server` keeps its own copy for now.

```bash
# shellcheck shell=bash
# Sourced by a personal service's ./serve to (re)start its server in a pane
# of a detached tmux session, in place, then return. Usage:
#
#   SESSION=rulinky APP_DIR=$(cd "$(dirname "$0")" && pwd)
#   . ~/c/server/serve-in-place.sh
#   serve_pane web 3002 "env RAILS_ENV=production bin/rails server -p 3002" /
#   serve_attach
#
# Why in place: git-sync runs ./activate -> ./serve after landing new code,
# with no tty. A foreground server would start a second copy beside the one
# in tmux. Pane commands go through top-cpu-service-wrapper, as
# after-reboot.sh's do, so top_cpu still attributes the server to its service.

: "${SESSION:?set SESSION before sourcing serve-in-place.sh}"
: "${APP_DIR:?set APP_DIR before sourcing serve-in-place.sh}"

# No login shell when git-sync runs us; tmux panes inherit this PATH below.
export PATH="$HOME/.rbenv/shims:$HOME/Library/pnpm:$HOME/.bun/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"

SERVE_WRAPPER="$HOME/c/server/top-cpu-service-wrapper"

port_listeners() {
  lsof -ti "tcp:$1" -sTCP:LISTEN 2>/dev/null || true
}

# The previous occupant of our port is always ours (including a foreground
# run of an old ./serve), so reclaim it before respawning the pane.
free_port() {
  local port="$1" pids
  pids=$(port_listeners "$port")
  [ -n "$pids" ] || return 0
  echo "==> Port $port held by PID(s): $(echo "$pids" | tr '\n' ' ')- stopping"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  for _ in $(seq 1 20); do
    sleep 0.5
    [ -n "$(port_listeners "$port")" ] || return 0
  done
  pids=$(port_listeners "$port")
  # shellcheck disable=SC2086
  [ -z "$pids" ] || kill -9 $pids 2>/dev/null || true
  sleep 1
  [ -z "$(port_listeners "$port")" ] || { echo "ERROR: could not free port $port."; return 1; }
}

pane_for() {
  tmux has-session -t "=$SESSION" 2>/dev/null || return 0
  tmux list-panes -t "=$SESSION:" -F '#{pane_title} #{pane_id}' 2>/dev/null |
    awk -v t="$1" '$1==t{print $2}'
}

pane_dead() {
  [ "$(tmux display-message -p -t "$1" '#{pane_dead}' 2>/dev/null)" = "1" ]
}

# start_pane <title> <argv...>: respawn the titled pane running argv through
# the wrapper; remain-on-exit keeps a pane that crashed on boot, with output.
start_pane() {
  local title="$1" cmd="$2" pid
  pid=$(pane_for "$title")
  if [ -z "$pid" ]; then
    if tmux has-session -t "=$SESSION" 2>/dev/null; then
      pid=$(tmux split-window -P -F '#{pane_id}' -t "=$SESSION:")
    else
      tmux new-session -d -s "$SESSION" -n main -x 200 -y 50
      pid=$(tmux list-panes -t "=$SESSION:" -F '#{pane_id}' | head -1)
    fi
    tmux select-pane -t "$pid" -T "$title"
  fi
  tmux set-option -t "=$SESSION:" remain-on-exit on >/dev/null
  tmux respawn-pane -k -t "$pid" \
    "cd '$APP_DIR' && export PATH='$PATH' && exec '$SERVE_WRAPPER' '$SESSION' '$APP_DIR' $cmd"
  tmux select-pane -t "$pid" -T "$title" # respawn-pane resets it on some tmux versions
  tmux select-layout -t "=$SESSION:" even-horizontal >/dev/null
}

dump_pane() {
  local pid
  pid=$(pane_for "$1")
  [ -n "$pid" ] || { echo "(no $1 pane to inspect)"; return 0; }
  echo "----- last lines of $1 pane -----"
  tmux capture-pane -p -S -500 -t "$pid" 2>/dev/null | grep -v '^$' | tail -n 25 || true
  echo "---------------------------------"
}

# wait_up <title> <port> <path>: up once the path answers below 500.
wait_up() {
  local title="$1" port="$2" path="$3" pid code
  pid=$(pane_for "$title")
  echo "==> Waiting for $title on port $port..."
  for _ in $(seq 1 60); do
    code=$(curl -s -o /dev/null -m 2 -w '%{http_code}' "http://127.0.0.1:$port$path" || true)
    if [ "$code" != 000 ] && [ "$code" -lt 500 ]; then
      echo "==> $title up (HTTP $code on $path)"
      return 0
    fi
    if [ -z "$pid" ] || pane_dead "$pid"; then
      echo "ERROR: $title exited during boot."
      dump_pane "$title"
      return 1
    fi
    sleep 1
  done
  echo "ERROR: $title not up after 60s."
  dump_pane "$title"
  return 1
}

# serve_pane <title> <port> <command> [<health path>]
serve_pane() {
  local title="$1" port="$2" cmd="$3" path="${4:-/}"
  echo "==> (Re)starting $title (port $port)"
  free_port "$port" || return 1
  start_pane "$title" "$cmd"
  wait_up "$title" "$port" "$path"
}

# serve_aux <title> <command>: a pane with no port to wait on (a watcher).
serve_aux() {
  echo "==> (Re)starting $1"
  start_pane "$1" "$2"
}

# Attach only when a human is watching; git-sync has no tty.
serve_attach() {
  if [ -n "${TMUX:-}" ] && [ -t 1 ]; then
    tmux switch-client -t "=$SESSION"
  elif [ -t 1 ]; then
    tmux attach-session -t "=$SESSION"
  else
    echo "==> No TTY; not attaching. Run: tmux attach -t $SESSION"
  fi
}
```

- [ ] **Step 2: Check and commit** (in `~/c/server`)

```bash
shellcheck -s bash serve-in-place.sh
git add serve-in-place.sh && git commit -m "feat: shared restart-in-place helper for personal services' ./serve"
```

### Task 8: `rulinky/serve`

**Files:** Rewrite `~/c/rulinky/serve`; create `~/c/rulinky/activate`.

- [ ] **Step 1: New `serve`**

```bash
#!/usr/bin/env bash
# (Re)start rulinky in tmux session "rulinky", in place, and return. Run by
# ../server/after-reboot.sh at boot and by ./activate after git-sync lands
# new code. Production env is set here, not inherited from ~/.zshrc.
set -euo pipefail
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$APP_DIR"
SESSION=rulinky
# shellcheck source=/dev/null
. "$HOME/c/server/serve-in-place.sh"
export RAILS_ENV=production

echo "==> bundle install";  bundle install --quiet
echo "==> pnpm install";    pnpm install --frozen-lockfile
echo "==> pnpm run build";  pnpm run build
echo "==> db:migrate";      bin/rails db:migrate

serve_pane web 3002 "env RAILS_ENV=production bin/rails server -p 3002"
serve_attach
```

- [ ] **Step 2: `activate`**

```sh
#!/bin/sh
# Run by git-sync after it lands new commits here. Restart the service on the
# new code, but only on the machine where it already runs (the Mac mini).
# No login shell under git-sync, and tmux lives in Homebrew.
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
tmux has-session -t "=rulinky" 2>/dev/null || exit 0
exec "$(dirname "$0")/serve"
```

- [ ] **Step 3: Check**

`chmod +x activate serve && shellcheck serve activate`, and `./activate` on a laptop, which should exit 0 at once.

- [ ] **Step 4: Ask the user, then test on the Mac mini.** Run `command ssh grillermo@192.168.1.1 '~/c/rulinky/serve'` *before* committing, using the not-yet-synced copy via `scp`, or after committing. Expected `==> web up (HTTP 2xx/3xx on /)`, and the app still answers on its public hostname.
- [ ] **Step 5: Commit**

```bash
git add serve activate && git commit -m "feat: restart in place via ./serve; add ./activate for git-sync"
```

Then watch the Mac mini's `git-sync report` for the `activate ok` event.

### Task 9: `serve-html-markdown/serve`

**Files:** Rewrite `~/c/serve-html-markdown/serve`; create `activate` (Task 8's, with session `serve-html-markdown`).

- [ ] **Step 1: New `serve`**

```bash
#!/usr/bin/env bash
# (Re)start serve-html-markdown in tmux session "serve-html-markdown", in
# place, and return: the web server and the files/ watcher in two panes.
# Run by ../server/after-reboot.sh at boot and by ./activate after git-sync
# lands new code.
set -euo pipefail
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$APP_DIR"
SESSION=serve-html-markdown
# shellcheck source=/dev/null
. "$HOME/c/server/serve-in-place.sh"
export RAILS_ENV=production
# The Solid Queue supervisor forks; on macOS the ObjC runtime aborts the child
# unless this is set, taking Puma down with it.
export OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES

echo "==> bundle install"; bundle install --quiet
echo "==> db:migrate";     bin/rails db:migrate
# Digested assets are cached for a year; clobber drops digests no source matches.
bin/rails assets:clobber
bin/rails assets:precompile

ENV_PREFIX="env RAILS_ENV=production OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES"
# bin/watch keeps served_files in sync; /last depends on it.
serve_aux watch "$ENV_PREFIX bin/watch"
serve_pane web 8009 "$ENV_PREFIX sh -c 'bin/rails server -p 8009 2>&1 | tee -a log/development.log'"
serve_attach
```

Behaviour change to flag to the user: the old `parallel --halt now,done=1` stopped the web server when the watcher died. Now they are separate panes, and a dead watcher stays visible as a dead pane (`remain-on-exit`) without taking the web server down.

- [ ] **Steps 2–5:** same as Task 8 Steps 2–5, using session name `serve-html-markdown`.

### Task 10: `ntfyllermo/serve`

**Files:** Rewrite `~/c/ntfyllermo/serve`; create `activate` (Task 8's, with session `ntfyllermo`).

- [ ] **Step 1: New `serve`**

```bash
#!/usr/bin/env bash
# (Re)start the ntfy server on :5333 in tmux session "ntfyllermo", in place,
# and return. :5333 is where cloudflared's ntfy.chiq.me hostname points.
#
# bin/ntfy is NOT the Homebrew/GitHub-release ntfy: those are built with
# `-tags noserver` and only ship the client. This one is built from upstream's
# `make cli-darwin-server` target (see README.md), so it has serve/user/access.
set -euo pipefail
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$APP_DIR"
SESSION=ntfyllermo
# shellcheck source=/dev/null
. "$HOME/c/server/serve-in-place.sh"

# Web push VAPID keys (NTFY_WEB_PUSH_*) are secrets in .env; the pane
# re-reads it, since tmux panes do not inherit this script's environment.
serve_pane ntfy 5333 "sh -c 'set -a; [ ! -f .env ] || . ./.env; set +a; exec ./bin/ntfy serve --config ./server.yml'" /v1/health
serve_attach
```

- [ ] **Steps 2–5:** same as Task 8 Steps 2–5, using session name `ntfyllermo`. Also update the `serve` line in `README.md`: it's no longer a "foreground start script".

### Task 11: `file_server` and `ipad-send` (no `serve` changes)

Both `serve` scripts already restart in place and skip attaching without a tty.

- [ ] **Step 1:** Create `~/c/file_server/activate` and `~/c/ipad-send/activate` from Task 8's template, with sessions `file_server` and `ipad-send`.
- [ ] **Step 2:** Note for the user: `ipad-send/serve` runs `git pull --ff-only github main` itself. Under git-sync that's a harmless no-op, since the tree is already at the remote's head.
- [ ] **Step 3:** shellcheck, ask the user, commit each repo, then confirm the `activate ok` event on the Mac mini.

### Task 12: Close out

- [ ] Update `~/c/git-sync/docs/superpowers/specs/2026-10-09-activate-design.md`'s open point about `top-cpu-service-wrapper`: resolved, because pane commands now run through the wrapper.
- [ ] Ask the user whether `file_server/serve` should move onto `serve-in-place.sh` too. It would also gain the wrapper. That's a separate change.
