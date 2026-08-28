# git-sync: sync the remote's default branch, not the checked-out one

> **For agentic workers:** implement with superpowers:executing-plans /
> superpowers:test-driven-development. Steps use checkbox (`- [ ]`) syntax for
> tracking. Each task writes the failing test **first**, then the code, and
> ends green. Run `make check` (gofmt + `go vet` + `go test ./...`) after every
> task and `make build` once the whole thing is green.

**Goal:** anchor every part of git-sync to the repo's shared line of history —
the **remote's default branch** (`main`, sometimes `master`/`trunk`) resolved
per repo — instead of whatever branch happens to be checked out. Push, receive
and install's initial sync all target the default branch regardless of the
working-tree checkout (feature branch or detached HEAD included). Non-default
branches never sync. Install first levels both machines' default branch against
the shared remote and, new, resolves a *clean* divergence by an automatic
merge; a merge that conflicts is aborted and reported, exactly as divergence is
reported today.

**Spec anchor:** amends `docs/superpowers/specs/2026-08-22-git-sync-design.md`.
The previous behaviour (sync the checked-out branch; never auto-merge anywhere)
is superseded by the decisions below.

---

## Design decisions (locked with the user)

1. **Target = the remote's default branch, resolved per repo.** Not the literal
   name `main`; resolve the remote's HEAD so `master`/`trunk` repos work.
2. **Everywhere.** push, receive and initial sync all target the default
   branch. A commit on any other branch leaves the default branch unmoved, so
   nothing syncs — that is the whole "non-default branches don't sync" rule,
   and it falls out for free rather than needing a guard.
3. **Auto-merge on divergence is install-time only.** During install, the
   *diverged* machine attempts a real merge of `<remote>/<branch>`; a clean
   merge is kept, pushed, and fast-forwarded on the other side; a conflicting
   merge is `git merge --abort`ed and reported for the user. Steady-state
   `receive` stays **fast-forward-only** — auto-merging there lets both
   machines mint rival merge commits and re-diverge forever, and receive never
   pushes so it cannot converge alone.
4. **No local default branch** (only feature branches, no local `main`): skip
   and report. Never auto-create or checkout a branch.
5. **Auto-merge only when the default branch is the checked-out HEAD.** Merging
   a non-checked-out branch needs a throwaway worktree; not worth it. A diverged
   *and* not-checked-out default branch is reported blocked. Fast-forwarding a
   non-checked-out branch is still done (safe `update-ref`).

---

## Files touched

```
internal/gitcmd/gitcmd.go        — DefaultBranch, HasLocalBranch, FastForwardRef, Merge, MergeAbort (+ sentinel)
internal/gitcmd/gitcmd_test.go   — tests for the above
internal/syncer/push.go          — push the default branch, drop detached-HEAD skip
internal/syncer/push_test.go
internal/syncer/receive.go       — receive onto the default branch via shared advance helper
internal/syncer/receive_test.go
internal/syncer/e2e_test.go       — feature-branch-checked-out end-to-end case
internal/setup/initialsync.go    — default-branch measurement + install-only clean-merge convergence
internal/setup/initialsync_test.go (or the existing setup test files)
```

`internal/setup/repocheck.go` resolves the remote by the same rule and compares
remote **URLs**, not branches — **no change needed**.

---

## Task 1: `gitcmd` default-branch primitives

**Files:** `internal/gitcmd/gitcmd.go`, `internal/gitcmd/gitcmd_test.go`

- [ ] **Step 1 — failing tests.** In `gitcmd_test.go`, using `testutil.NewSandbox`:
  - `TestDefaultBranchFromCloneHead`: `MakeRepo("a")` → `DefaultBranch(dir,
    "origin")` == `"main"` (clone sets `refs/remotes/origin/HEAD`).
  - `TestDefaultBranchAfterRemoteRename`: `MakeRepoNamedRemote("a","github")` →
    `DefaultBranch(dir,"github")` == `"main"`.
  - `TestDefaultBranchSetHeadFallback`: `MakeRepo`, then `AddRemote(t,dir,
    "github","a")` (a `git remote add` leaves `refs/remotes/github/HEAD`
    unset) → `DefaultBranch(dir,"github")` still resolves (exercises the
    `remote set-head` fallback).
  - `TestFastForwardRefAdvancesNonCheckedOutBranch`: on a repo with a feature
    branch checked out and `origin/main` ahead, `FastForwardRef(dir,"origin",
    "main")` moves local `main` to `origin/main` without touching the worktree;
    a diverged `main` returns an error and does not move the ref.
  - `TestHasLocalBranch`: true for `main`, false for a name with no local ref.

- [ ] **Step 2 — implement.** Add to `gitcmd.go`:

```go
var errNoDefaultBranch = errors.New("no default branch")

// IsNoDefaultBranch reports whether err means the remote's default branch
// could not be resolved. Distinct from IsNoRemote: the remote is fine, its
// HEAD just isn't known and couldn't be fetched.
func IsNoDefaultBranch(err error) bool { return errors.Is(err, errNoDefaultBranch) }

// DefaultBranch returns the remote's default branch (bare, e.g. "main"),
// resolved once and cached in refs/remotes/<remote>/HEAD. This is where the
// two machines meet, independent of what is checked out on either side.
func DefaultBranch(dir, remote string) (string, error) {
	if b, err := headRef(dir, remote); err == nil {
		return b, nil
	}
	// refs/remotes/<remote>/HEAD is set by clone and remote rename but not by
	// a bare `remote add`; ask the remote once and cache it.
	_, _ = Run(dir, "remote", "set-head", remote, "-a")
	b, err := headRef(dir, remote)
	if err != nil {
		return "", fmt.Errorf("%w: %s on %s", errNoDefaultBranch, remote, dir)
	}
	return b, nil
}

func headRef(dir, remote string) (string, error) {
	out, err := Run(dir, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(out, remote+"/"), nil
}

// HasLocalBranch reports whether the repo has a local branch of this name.
// A repo with only feature branches and no local default branch is skipped,
// never auto-created.
func HasLocalBranch(dir, branch string) bool {
	_, err := Run(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// FastForwardRef advances a branch that is NOT checked out to <remote>/<branch>
// without touching the working tree. update-ref refuses nothing on its own, so
// callers must have already confirmed this is a pure fast-forward
// (behind>0 && ahead==0); a plain reset here would silently drop commits.
func FastForwardRef(dir, remote, branch string) error {
	_, err := Run(dir, "update-ref", "refs/heads/"+branch, remote+"/"+branch)
	return err
}

// Merge performs a real (non-ff) merge of <remote>/<branch> into HEAD. Used
// only by install-time convergence, never by receive. Returns an error on
// conflicts; the caller then MergeAbort()s.
func Merge(dir, remote, branch string) error {
	_, err := Run(dir, "merge", "--no-edit", remote+"/"+branch)
	return err
}

func MergeAbort(dir string) error {
	_, err := Run(dir, "merge", "--abort")
	return err
}
```

- [ ] **Step 3 — green.** `go test ./internal/gitcmd/...`.

---

## Task 2: `receive` onto the default branch

**Files:** `internal/syncer/receive.go`, `internal/syncer/receive_test.go`

- [ ] **Step 1 — failing tests.** Extend `receive_test.go`:
  - `TestReceiveFastForwardsDefaultBranchWithFeatureCheckedOut`: peer pushes to
    `main`; the receiving repo has a feature branch checked out; `Receive`
    fast-forwards local `main` (via `FastForwardRef`) and leaves the worktree /
    feature branch untouched; event is `StatusOK`.
  - `TestReceiveFastForwardsDefaultBranchWhenItIsCheckedOut`: default branch is
    HEAD and clean → stash-free `merge --ff-only`; dirty → stash/pop dance
    (assert restored). Adapt the existing checked-out-branch tests to `main`.
  - `TestReceiveDetachedHeadStillSyncsDefaultBranch`: detached HEAD no longer
    skips; local `main` advances.
  - `TestReceiveDivergedDefaultBranchWarnsNoAutoMerge`: diverged `main` →
    `StatusWarn` "manual merge needed", nothing merged (receive is ff-only).
  - `TestReceiveNoLocalDefaultBranchSkips`: repo with only a feature branch,
    no local `main` → skip+report.

- [ ] **Step 2 — implement.** In `syncRepo`, replace the `CurrentBranch` anchor
  with the default branch and route through a shared advance helper. Sketch:

```go
remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
if err != nil { log(StatusWarn, "", "no remote to sync through: "+firstLine(err.Error())); return 0 }

branch, err := gitcmd.DefaultBranch(dir, remote)
if err != nil { log(StatusWarn, "", "could not resolve default branch on "+remote); return 0 }

if !gitcmd.HasLocalBranch(dir, branch) {
	log(StatusSkip, branch, "no local "+branch+" branch, skipping"); return 0
}
if err := gitcmd.Fetch(dir, remote); err != nil { /* existing error path */ }
if !gitcmd.HasRemoteBranch(dir, remote, branch) { /* existing "not on remote" skip */ }

ahead, behind, err := gitcmd.AheadBehind(dir, remote, branch)
// behind==0 -> already level (log OK "up to date")
// ahead>0 && behind>0 -> StatusWarn "diverged ... manual merge needed" (NO auto-merge)
// else fast-forward:
head, _ := gitcmd.CurrentBranch(dir) // "" on detached
if head == branch {
	// default branch is checked out: today's stash / merge --ff-only / unconditional pop
} else {
	// not checked out: gitcmd.FastForwardRef(dir, remote, branch)
}
```

  Keep the stash/pop discipline byte-for-byte where the default branch **is**
  HEAD (unconditional pop, its failure outranks the merge result). The
  `update-ref` branch needs no stash — it never touches the worktree.

- [ ] **Step 3 — green.** `go test -race ./internal/syncer/... -run TestReceive`.

---

## Task 3: `push` the default branch

**Files:** `internal/syncer/push.go`, `internal/syncer/push_test.go`

- [ ] **Step 1 — failing tests.** Extend `push_test.go`:
  - `TestPushSendsDefaultBranchWithFeatureCheckedOut`: commit on `main`, feature
    branch checked out → `git push <remote> main`, peer notified with
    `branch=main`.
  - `TestPushOnFeatureBranchPushesNothing`: commit only on a feature branch
    (default branch unmoved) → push is a no-op ("Everything up-to-date"),
    `StatusOK`, no error event.
  - `TestPushDetachedHeadSyncsDefaultBranch`: detached HEAD no longer skips.
  - `TestPushNoLocalDefaultBranchSkips`: no local `main` → `StatusSkip`.

- [ ] **Step 2 — implement.** Reorder so the remote is resolved first, then the
  default branch; drop the detached-HEAD early-return:

```go
remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
if err != nil { /* existing StatusWarn "no remote" */ }
branch, err := gitcmd.DefaultBranch(dir, remote)
if err != nil { /* StatusWarn "could not resolve default branch" ; return 0 */ }
if !gitcmd.HasLocalBranch(dir, branch) { /* StatusSkip "no local <branch>" ; return 0 */ }
if _, err := gitcmd.Push(dir, remote, branch); err != nil { /* existing StatusError */ }
// existing OK event + notify(cfg, rel, branch)
```

- [ ] **Step 3 — green.** `go test -race ./internal/syncer/... -run TestPush`.

---

## Task 4: `install` initial sync — default branch + clean-merge convergence

**Files:** `internal/setup/initialsync.go` and its test file(s)

- [ ] **Step 1 — failing tests.** Add to the setup tests:
  - `TestInitialSyncLevelsDefaultBranchWithFeatureCheckedOut`: both machines on
    a feature branch, one ahead on `main` → after `ApplySync` both machines'
    `main` are the same commit; the "different branches" blocker never fires.
  - `TestInitialSyncCleanMergeConverges`: `main` genuinely diverged but the
    changes don't conflict → the diverged side merges, pushes, the other
    fast-forwards; final measure shows both level; nothing left blocked.
  - `TestInitialSyncConflictAbortsAndReports`: `main` diverged with a real
    conflict → `merge --abort`, working tree restored (assert not dirty, no new
    commit), repo reported blocked/diverged for manual merge.
  - `TestInitialSyncPeerSideCleanMerge`: the *peer* is the diverged side → the
    ssh `syncApplyScript` merges and pushes; local `main` fast-forwards in
    pass 3.

- [ ] **Step 2 — implement (Go half).**
  - `measureHere`: resolve `DefaultBranch(dir, remote)` (not `CurrentBranch`);
    if `!HasLocalBranch` set `Err: "no local <branch> branch"`; measure that
    branch's ahead/behind. Everything downstream (`SyncPos`, `diverged`,
    `canPush`, `canFF`) is unchanged — it already operates on `Branch`.
  - `blocked()`: the `Here.Branch != There.Branch` clause now almost never
    fires (both resolve the same remote's default). Keep it as a defensive
    guard.
  - `ApplySync`: after the existing push (pass 1) / peer push+ff (pass 2) /
    local ff (pass 3), add a **clean-merge** step for the local (`Here`)
    diverged case, only when the default branch is checked out:
    `landMerge(dir, remote, branch)` = stash-if-dirty → `gitcmd.Merge`; on
    error `gitcmd.MergeAbort` + return a note leaving it blocked; on success
    `gitcmd.Push`, then unconditional stash pop (reuse `landHere`'s discipline).
    The peer's diverged case is handled in the shell (below); pass 3's local ff
    then lands whatever the peer merged and pushed.

- [ ] **Step 3 — implement (shell mirror).**
  - `syncScript`: replace `symbolic-ref --short HEAD` with default-branch
    resolution and prefix strip:

    ```sh
    b=$(git -C "$d" symbolic-ref --short "refs/remotes/$r/HEAD" 2>/dev/null) \
      || { git -C "$d" remote set-head "$r" -a >/dev/null 2>&1; \
           b=$(git -C "$d" symbolic-ref --short "refs/remotes/$r/HEAD" 2>/dev/null); }
    b=${b#$r/}
    if [ -z "$b" ]; then echo "err $rel could not resolve default branch"; continue; fi
    if ! git -C "$d" rev-parse --verify --quiet "refs/heads/$b" >/dev/null; then
      echo "err $rel no local $b branch"; continue
    fi
    ```

    (`$r` is already the resolved remote from the existing loop.) Note: the
    default branch may not be `$b`-checked-out; the measure counts are still
    correct because `rev-list --left-right --count "$r/$b...$b"` reads refs, not
    HEAD.
  - `syncApplyScript`: keep the existing "purely ahead → push / purely behind →
    stash+`merge --ff-only`+pop" block, but guard the merge on the default
    branch being HEAD (`[ "$(git -C "$d" symbolic-ref --short HEAD 2>/dev/null)" = "$b" ]`);
    when it isn't, fast-forward the ref instead:
    `git -C "$d" update-ref "refs/heads/$b" "$r/$b"`. Then add a diverged
    branch — only when `$b` is HEAD:

    ```sh
    if [ "$ahead" -gt 0 ] && [ "$behind" -gt 0 ] \
       && [ "$(git -C "$d" symbolic-ref --short HEAD 2>/dev/null)" = "$b" ]; then
      stashed=no
      [ -n "$(git -C "$d" status --porcelain)" ] && \
        git -C "$d" stash push -u -m 'git-sync initial sync' >/dev/null 2>&1 && stashed=yes
      if git -C "$d" merge --no-edit "$r/$b" >/dev/null 2>&1; then
        git -C "$d" push -q "$r" "$b" >/dev/null 2>&1 || true
      else
        git -C "$d" merge --abort >/dev/null 2>&1 || true
      fi
      if [ "$stashed" = yes ] && ! git -C "$d" stash pop >/dev/null 2>&1; then
        echo "note $rel uncommitted changes are safe in the peer's git stash list but conflicted on the way back"
      fi
    fi
    ```

- [ ] **Step 4 — wording.** Update `describePos` / `blockedReason` /
  `leftoverReason` copy that says "branch checked out" to talk about the
  default branch where it now reads wrong. A still-diverged repo (conflict
  aborted) keeps the existing "history has diverged; merge it by hand" line.

- [ ] **Step 5 — green.** `go test ./internal/setup/...`.

---

## Task 5: end-to-end + docs + rebuild

**Files:** `internal/syncer/e2e_test.go`, `CLAUDE.md`, spec/plan docs, `./git-sync`

- [ ] **Step 1 — e2e test.** Add a case to `e2e_test.go`: both machines check
  out a feature branch, commit on the default branch, and the loopback-ssh
  round trip converges the default branch end-to-end. Reuse `buildBinary`,
  `peerMachine`, `installLoopbackSSH`.

- [ ] **Step 2 — docs.** Update `CLAUDE.md` (the receive/push/initialsync
  descriptions and the "one-sided repo / different branches" invariant) and the
  design spec to describe default-branch anchoring and the install-only
  clean-merge. Note the one softened invariant: install *may* auto-merge a
  **clean** divergence; conflicts and steady-state divergence are still never
  auto-merged.

- [ ] **Step 3 — full verification.**

```bash
make check                              # gofmt + vet + go test ./...
go test -race ./internal/syncer/...     # push/receive concurrency + e2e
go test ./internal/setup/... ./internal/gitcmd/...
make build                              # refresh the checked-in ./git-sync
```

- [ ] **Step 4 — manual smoke (optional).** On a real repo checked out on a
  feature branch with the default branch behind its remote, run
  `git-sync install <base>` and confirm the report levels the default branch,
  and that the next commit on the default branch fast-forwards on the peer.

---

## Risks & notes

- **`remote set-head -a` is a network call.** It runs at most once per repo
  (the ref is then cached), and only when clone/rename didn't already set it.
  Acceptable: install and receive already fetch.
- **`update-ref` is unguarded by git.** Only call `FastForwardRef` after
  confirming `behind>0 && ahead==0`; the tests must cover the guard, because a
  bare `update-ref` on a diverged branch would drop the local-only commits.
- **Merge commits are created only on the install machine / diverged side, once.**
  Because only the diverged side merges and the other fast-forwards, the two
  machines cannot mint rival merge commits — the failure mode that keeps
  auto-merge out of steady-state `receive`.
- Test sandboxing rules from `CLAUDE.md` still hold: `testutil.NewSandbox`, no
  `t.Parallel()`, `-race` required for `internal/syncer`.
