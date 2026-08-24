// Package testutil provides sandboxed fixtures for git-sync's tests.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
)

// Sandbox is an isolated fake machine: its own HOME, GITSYNC_HOME, git global
// config and base_dir.
type Sandbox struct {
	Home        string // fake $HOME
	GitsyncHome string // fake ~/.gitsync
	BaseDir     string // the sync root
	T           *testing.T
}

// NewSandbox creates a sandbox and points this process's env at it.
// Uses t.Setenv, so it is restored automatically and forbids t.Parallel.
func NewSandbox(t *testing.T) *Sandbox {
	t.Helper()
	home := t.TempDir()
	sb := &Sandbox{
		Home:        home,
		GitsyncHome: filepath.Join(home, ".gitsync"),
		BaseDir:     filepath.Join(home, "code"),
		T:           t,
	}
	MkdirAll(t, sb.GitsyncHome, sb.BaseDir)

	t.Setenv("HOME", sb.Home)
	t.Setenv("GITSYNC_HOME", sb.GitsyncHome)
	// Never the real keychain: it is shared OS state, and reading it on macOS
	// can raise a GUI prompt in the middle of a test run.
	t.Setenv("GITSYNC_SECRET_BACKEND", "file")
	// Belt and braces: HOME alone redirects the global config on git >= 2.32,
	// but be explicit so a stray HOME leak can never write the real one.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(sb.Home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "git-sync test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "git-sync test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	return sb
}

// Git runs a git command in dir and fails the test if it errors.
func (sb *Sandbox) Git(dir string, args ...string) string {
	sb.T.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		sb.T.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// MakeRepo creates a bare origin plus a working clone at BaseDir/rel with one
// commit on main and an upstream configured. Returns the clone's path.
func (sb *Sandbox) MakeRepo(rel string) string {
	sb.T.Helper()
	origin := filepath.Join(sb.Home, "remotes", rel+".git")
	work := filepath.Join(sb.BaseDir, rel)
	MkdirAll(sb.T, filepath.Dir(origin), filepath.Dir(work))

	sb.Git(sb.Home, "init", "-q", "--bare", "--initial-branch=main", origin)
	sb.Git(sb.Home, "clone", "-q", origin, work)
	WriteFileIn(sb.T, work, "README.md", "initial\n")
	sb.Git(work, "add", "-A")
	sb.Git(work, "commit", "-qm", "initial")
	sb.Git(work, "push", "-q", "-u", "origin", "main")
	return work
}

// MakeRepoNamedRemote is MakeRepo followed by renaming the "origin" remote to
// remote. The sync goes through the remote, so its name is a variable worth
// testing.
func (sb *Sandbox) MakeRepoNamedRemote(rel, remote string) string {
	sb.T.Helper()
	work := sb.MakeRepo(rel)
	if remote != "origin" {
		sb.Git(work, "remote", "rename", "origin", remote)
	}
	return work
}

// AddRemote creates a second bare repo (a clone of repoDir's current state)
// and adds it to repoDir under name. Returns the bare repo's path. originRel
// only names the new bare repo's path under remotes/, keeping it distinct
// from rel's own origin.
func (sb *Sandbox) AddRemote(t *testing.T, repoDir, name, originRel string) string {
	t.Helper()
	bare := filepath.Join(sb.Home, "remotes", originRel+"-"+name+".git")
	MkdirAll(t, filepath.Dir(bare))
	sb.Git(sb.Home, "clone", "-q", "--bare", repoDir, bare)
	sb.Git(repoDir, "remote", "add", name, bare)
	return bare
}

// PeerClone makes a second clone of rel's origin, standing in for the copy on
// the other machine. Returns its path.
func (sb *Sandbox) PeerClone(rel string) string {
	sb.T.Helper()
	dst := filepath.Join(sb.Home, "peer", rel)
	MkdirAll(sb.T, filepath.Dir(dst))
	sb.Git(sb.Home, "clone", "-q", filepath.Join(sb.Home, "remotes", rel+".git"), dst)
	return dst
}

// PeerCommit makes and pushes a commit from the peer clone: "the other machine
// committed something". Writes to PEER.md rather than README.md so a peer
// commit never collides by construction with Dirty's tracked-file edit: two
// independent appends to the same file at the same base revision always
// conflict on `stash pop`, and callers that want that conflict do it
// explicitly by editing the same file the peer touched.
func (sb *Sandbox) PeerCommit(rel, msg string) {
	sb.T.Helper()
	dst := filepath.Join(sb.Home, "peer", rel)
	AppendFileIn(sb.T, dst, "PEER.md", msg+"\n")
	sb.Git(dst, "add", "-A")
	sb.Git(dst, "commit", "-qm", msg)
	sb.Git(dst, "push", "-q")
}

// StubSSH puts a fake ssh first on PATH. It appends its arguments to
// GitsyncHome/ssh-calls.log and exits with exitCode.
func (sb *Sandbox) StubSSH(exitCode int) {
	sb.T.Helper()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$GITSYNC_HOME/ssh-calls.log\"\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	sb.installSSHStub(script)
}

// StubSSHFailing is a fake ssh that always fails with the given exit code and
// stderr message. "Permission denied" vs. a connection error is a distinction
// install must make.
func (sb *Sandbox) StubSSHFailing(code int, stderrMsg string) {
	sb.T.Helper()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$GITSYNC_HOME/ssh-calls.log\"\n" +
		"echo " + shellQuote(stderrMsg) + " >&2\n" +
		"exit " + strconv.Itoa(code) + "\n"
	sb.installSSHStub(script)
}

// StubSSHPassword is a fake ssh that rejects every attempt until
// $SSH_ASKPASS yields want, at which point it succeeds.
func (sb *Sandbox) StubSSHPassword(want string) {
	sb.T.Helper()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$GITSYNC_HOME/ssh-calls.log\"\n" +
		"if [ -z \"$SSH_ASKPASS\" ]; then\n" +
		"  echo 'Permission denied (publickey,password).' >&2\n" +
		"  exit 5\n" +
		"fi\n" +
		"got=$(\"$SSH_ASKPASS\" 2>/dev/null)\n" +
		"if [ \"$got\" != " + shellQuote(want) + " ]; then\n" +
		"  echo 'Permission denied, please try again.' >&2\n" +
		"  exit 5\n" +
		"fi\n" +
		"exit 0\n"
	sb.installSSHStub(script)
}

// StubSSHScripted installs a fake ssh that answers the two probe commands
// provisioning sends, records every invocation, and streams any stdin it is
// given to GitsyncHome/ssh-stdin-<n>. replies maps a substring of the remote
// command to the stdout the stub should produce.
func (sb *Sandbox) StubSSHScripted(replies map[string]string, exitCode int) {
	sb.T.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("printf '%s\\n' \"$*\" >> \"$GITSYNC_HOME/ssh-calls.log\"\n")
	b.WriteString("n=0\n")
	b.WriteString("while [ -e \"$GITSYNC_HOME/ssh-stdin-$n\" ]; do n=$((n+1)); done\n")
	b.WriteString("if [ ! -t 0 ]; then cat > \"$GITSYNC_HOME/ssh-stdin-$n\"; fi\n")
	b.WriteString("case \"$*\" in\n")
	for substr, reply := range replies {
		fmt.Fprintf(&b, "  *%s*) printf '%%s' %s ;;\n", shellGlobEscape(substr), shellQuote(reply))
	}
	b.WriteString("esac\n")
	fmt.Fprintf(&b, "exit %d\n", exitCode)
	sb.installSSHStub(b.String())
}

// shellGlobEscape escapes characters that are special inside a `case`
// pattern: glob metacharacters, and "$" and backslash, which would otherwise
// undergo parameter expansion - so a literal substring like "$HOME" matches
// itself rather than being expanded to the stub's own $HOME.
func shellGlobEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "$", `\$`, "*", `\*`, "?", `\?`, "[", `\[`)
	return r.Replace(s)
}

// SSHStdin returns what the stub captured on stdin for the ssh call whose
// logged command line contains nameSubstring.
func (sb *Sandbox) SSHStdin(t *testing.T, nameSubstring string) string {
	t.Helper()
	calls := strings.Split(strings.TrimRight(sb.SSHCalls(), "\n"), "\n")
	for i, call := range calls {
		if strings.Contains(call, nameSubstring) {
			b, err := os.ReadFile(filepath.Join(sb.GitsyncHome, fmt.Sprintf("ssh-stdin-%d", i)))
			if err != nil {
				return ""
			}
			return string(b)
		}
	}
	return ""
}

func (sb *Sandbox) installSSHStub(script string) {
	sb.T.Helper()
	bin := filepath.Join(sb.Home, "bin")
	MkdirAll(sb.T, bin)
	path := filepath.Join(bin, "ssh")
	writeExecutable(sb.T, path, script)
	sb.T.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// SSHCalls returns the recorded ssh invocations, or "" if there were none.
func (sb *Sandbox) SSHCalls() string {
	b, err := os.ReadFile(filepath.Join(sb.GitsyncHome, "ssh-calls.log"))
	if err != nil {
		return ""
	}
	return string(b)
}

// Dirty leaves an uncommitted tracked edit and an untracked file in repoDir.
func (sb *Sandbox) Dirty(repoDir string) {
	sb.T.Helper()
	AppendFileIn(sb.T, repoDir, "README.md", "uncommitted edit\n")
	WriteFileIn(sb.T, repoDir, "NOTES.md", "work in progress\n")
}

// Commit appends msg to README.md in repoDir, stages everything and commits.
func Commit(t *testing.T, sb *Sandbox, repoDir, msg string) {
	t.Helper()
	AppendFileIn(t, repoDir, "README.md", msg+"\n")
	sb.Git(repoDir, "add", "-A")
	sb.Git(repoDir, "commit", "-qm", msg)
}

// WriteScript writes an executable script named name under sb.Home/bin and
// returns its path.
func WriteScript(t *testing.T, sb *Sandbox, name, body string) string {
	t.Helper()
	bin := filepath.Join(sb.Home, "bin")
	MkdirAll(t, bin)
	path := filepath.Join(bin, name)
	writeExecutable(t, path, body)
	return path
}

// SaveConfig writes a valid config.toml pointed at sb.BaseDir, with every
// repo currently under it selected (found by scanning for ".git" dirs).
func SaveConfig(t *testing.T, sb *Sandbox, peerHost, peerUser string) {
	t.Helper()
	SaveConfigWithRepos(t, sb, peerHost, peerUser, discoverRepos(t, sb))
}

// SaveConfigWithRepos writes a valid config.toml pointed at sb.BaseDir, with
// repos as the explicit allowlist.
func SaveConfigWithRepos(t *testing.T, sb *Sandbox, peerHost, peerUser string, repos []string) {
	t.Helper()
	cfg := config.Config{
		BaseDir:  sb.BaseDir,
		PeerHost: peerHost,
		PeerUser: peerUser,
		Repos:    repos,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

// SaveConfigWithRemotes writes a valid config.toml pointed at sb.BaseDir, with
// every repo currently under it selected (as SaveConfig does) plus an
// explicit remote-name preference list.
func SaveConfigWithRemotes(t *testing.T, sb *Sandbox, peerHost, peerUser string, remoteNames []string) {
	t.Helper()
	cfg := config.Config{
		BaseDir:     sb.BaseDir,
		PeerHost:    peerHost,
		PeerUser:    peerUser,
		Repos:       discoverRepos(t, sb),
		RemoteNames: remoteNames,
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("SaveConfigWithRemotes: %v", err)
	}
}

// discoverRepos scans sb.BaseDir for directories containing a .git entry and
// returns their paths relative to sb.BaseDir.
func discoverRepos(t *testing.T, sb *Sandbox) []string {
	t.Helper()
	var repos []string
	err := filepath.WalkDir(sb.BaseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
			rel, relErr := filepath.Rel(sb.BaseDir, path)
			if relErr != nil {
				return relErr
			}
			repos = append(repos, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discoverRepos: %v", err)
	}
	return repos
}

// AssertEvent fails the test unless activity.Read() contains an event with
// the given op, status and a Msg containing msgSubstr ("" matches any msg).
func AssertEvent(t *testing.T, op activity.Op, status activity.Status, msgSubstr string) {
	t.Helper()
	events, err := activity.Read()
	if err != nil {
		t.Fatalf("activity.Read: %v", err)
	}
	for _, e := range events {
		if e.Op == op && e.Status == status && strings.Contains(e.Msg, msgSubstr) {
			return
		}
	}
	t.Errorf("no event found with op=%s status=%s msg containing %q; got %+v", op, status, msgSubstr, events)
}

// AssertNoEvent fails the test if activity.Read() contains an event with the
// given op and status.
func AssertNoEvent(t *testing.T, op activity.Op, status activity.Status) {
	t.Helper()
	events, err := activity.Read()
	if err != nil {
		t.Fatalf("activity.Read: %v", err)
	}
	for _, e := range events {
		if e.Op == op && e.Status == status {
			t.Errorf("unexpected event with op=%s status=%s: %+v", op, status, e)
		}
	}
}

// MkdirAll makes every directory in dirs, failing the test on error.
func MkdirAll(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

// WriteFileIn writes name/content inside dir, creating dir if needed.
func WriteFileIn(t *testing.T, dir, name, content string) {
	t.Helper()
	MkdirAll(t, dir)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// AppendFileIn appends content to name inside dir, creating the file if
// needed.
func AppendFileIn(t *testing.T, dir, name, content string) {
	t.Helper()
	MkdirAll(t, dir)
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// AssertFileContains fails the test if the file at path does not contain
// substr.
func AssertFileContains(t *testing.T, path, substr string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), substr) {
		t.Errorf("%s does not contain %q:\n%s", path, substr, b)
	}
}

// SamePath reports whether a and b refer to the same file, comparing via
// filepath.EvalSymlinks so macOS's /var -> /private/var mapping doesn't
// produce false negatives from raw string comparison.
func SamePath(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}

// Chdir wraps t.Chdir, changing the working directory for the duration of
// the test and restoring it automatically afterward.
func Chdir(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// shellQuote wraps s in single quotes for embedding into a generated POSIX
// shell script literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
