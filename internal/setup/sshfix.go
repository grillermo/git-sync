package setup

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
)

// Problem is why one machine could not ssh to another, read off ssh's own
// error output. Each has a different fix, so "run ssh-copy-id" for all of them
// sends the user off to fix the wrong thing half the time.
type Problem int

const (
	ProblemOther Problem = iota
	// ProblemHostKeyUnknown: the target's host key is not in known_hosts yet,
	// and BatchMode cannot ask whether to trust it.
	ProblemHostKeyUnknown
	// ProblemHostKeyChanged: known_hosts has a *different* key for it - a
	// reinstalled machine, a reused IP, or someone in the middle.
	ProblemHostKeyChanged
	// ProblemAuth: the server is there but does not accept any of our keys.
	ProblemAuth
	// ProblemUnresolvable: the hostname does not resolve.
	ProblemUnresolvable
	// ProblemRefused: nothing is listening on port 22 - sshd is off.
	ProblemRefused
	// ProblemOffline: no answer at all.
	ProblemOffline
)

// diagnose classifies ssh's combined output. Order matters: a changed host
// key also prints "Host key verification failed".
func diagnose(out string) Problem {
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(out, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("REMOTE HOST IDENTIFICATION HAS CHANGED", "Offending"):
		return ProblemHostKeyChanged
	case has("Host key verification failed", "host key is known for", "No matching host key"):
		return ProblemHostKeyUnknown
	case has("Permission denied", "Too many authentication failures"):
		return ProblemAuth
	case has("Could not resolve hostname", "Name or service not known", "nodename nor servname"):
		return ProblemUnresolvable
	case has("Connection refused"):
		return ProblemRefused
	case has("timed out", "No route to host", "Network is unreachable", "Host is down"):
		return ProblemOffline
	}
	return ProblemOther
}

// Fix repairs one direction of the mesh. Steps are argv run in order on this
// machine's terminal - interactive on purpose, since accepting a host key
// and typing a password once are exactly what BatchMode cannot do. A fix
// with no Steps is something only the user can do; Hint says what.
type Fix struct {
	From, To config.Peer
	Problem  Problem
	Explain  string
	Steps    [][]string
	Hint     string
	// Risky marks a fix that throws away a recorded host key: it must be
	// confirmed on its own, never swept up in a "fix everything".
	Risky bool
}

// Commands renders Steps the way the user would type them.
func (f Fix) Commands() []string {
	out := make([]string, len(f.Steps))
	for i, s := range f.Steps {
		q := make([]string, len(s))
		for j, a := range s {
			q[j] = shellQuote(a)
		}
		out[i] = strings.Join(q, " ")
	}
	return out
}

// Reach checks that this machine can ssh to p, as a KeyResult from self so
// that it goes through the same FixesFor as the pairwise check.
func Reach(self, p config.Peer) KeyResult {
	r := KeyResult{From: self, To: p, OK: true}
	if err := Reachable(p.Target()); err != nil {
		r.OK, r.Err, r.Output = false, firstLine(err.Error()), err.Error()
	}
	return r
}

// FixesFor turns every failing result into a Fix. A pair run *on* a peer
// this machine cannot itself reach is not diagnosed: the error it carries is
// about getting to that peer, not about the pair, so its fix is to fix that
// first.
func FixesFor(self config.Peer, results []KeyResult) []Fix {
	unreachable := map[string]bool{}
	for _, r := range results {
		if !r.OK && isSelf(self, r.From) {
			unreachable[strings.ToLower(r.To.Host)] = true
		}
	}
	var out []Fix
	for _, r := range results {
		if r.OK {
			continue
		}
		if !isSelf(self, r.From) && unreachable[strings.ToLower(r.From.Host)] {
			out = append(out, Fix{
				From: r.From, To: r.To,
				Explain: fmt.Sprintf("not checked: this machine cannot reach %s itself", r.From.Host),
				Hint:    fmt.Sprintf("fix %s -> %s first, then run install again", self.Host, r.From.Host),
			})
			continue
		}
		out = append(out, fixFor(self, r))
	}
	return out
}

func fixFor(self config.Peer, r KeyResult) Fix {
	to := r.To
	f := Fix{From: r.From, To: to, Problem: diagnose(r.Output)}
	local := isSelf(self, r.From)
	on := "this machine"
	if !local {
		on = r.From.Host
	}

	// run puts steps in place: as they are here, or on r.From over a
	// terminal so its prompts reach the user.
	run := func(local [][]string, remote string) {
		if isSelf(self, r.From) {
			f.Steps = local
		} else {
			f.Steps = [][]string{{"ssh", "-t", r.From.Target(), remote}}
		}
	}
	// A host key problem is fixed by connecting once by hand, which asks
	// whether to trust the key (and shows any other name it is already known
	// by). ssh-copy-id is the fix for a rejected key, not this; if login
	// still fails afterwards, the next install says so on its own.
	accept := func(pre ...string) {
		steps := [][]string{}
		for _, p := range pre {
			steps = append(steps, strings.Fields(p))
		}
		steps = append(steps, []string{"ssh", to.Target(), "true"})
		remote := strings.Join(append(pre, "ssh "+shellQuote(to.Target())+" true"), " && ")
		run(steps, remote)
	}

	switch f.Problem {
	case ProblemHostKeyUnknown:
		f.Explain = fmt.Sprintf("%s has never seen %s's host key, and git-sync cannot answer ssh's "+
			"\"trust this host?\" prompt", on, to.Host)
		accept()
	case ProblemHostKeyChanged:
		f.Explain = fmt.Sprintf("%s has a DIFFERENT host key recorded for %s. Expected if that machine "+
			"was reinstalled or its address reused; otherwise it may be an impostor", on, to.Host)
		f.Risky = true
		accept("ssh-keygen -R " + to.Host)
	case ProblemAuth:
		f.Explain = fmt.Sprintf("%s does not accept any of %s's ssh keys", to.Host, on)
		run(copyIDSteps(to.Target()), remoteCopyID(to.Target()))
	case ProblemUnresolvable:
		f.Explain = fmt.Sprintf("%s cannot resolve the name %s", on, to.Host)
		f.Hint = fmt.Sprintf("use %s's IP address, or check that mDNS (.local names) works on %s", to.Host, on)
	case ProblemRefused:
		f.Explain = fmt.Sprintf("%s is up but not running an ssh server", to.Host)
		f.Hint = fmt.Sprintf("on %s - macOS: System Settings > General > Sharing > Remote Login; "+
			"Linux: sudo systemctl enable --now sshd", to.Host)
	case ProblemOffline:
		f.Explain = fmt.Sprintf("%s does not answer from %s", to.Host, on)
		f.Hint = fmt.Sprintf("check %s is awake and on the same network", to.Host)
	default:
		f.Explain = r.Err
		cmd := "ssh " + to.Target()
		if !local {
			cmd = "ssh -t " + r.From.Target() + " " + shellQuote(cmd)
		}
		f.Hint = "run `" + cmd + "` to see what ssh says"
	}
	return f
}

// RenderFix prints one fix: what is wrong, then the commands or the hint.
func RenderFix(w io.Writer, f Fix) {
	fmt.Fprintf(w, "  %s -> %s: %s\n", f.From.Host, f.To.Host, f.Explain)
	for _, c := range f.Commands() {
		fmt.Fprintf(w, "      $ %s\n", c)
	}
	if f.Hint != "" {
		fmt.Fprintf(w, "      %s\n", f.Hint)
	}
}

// RunFix runs f's steps in order through run, stopping at the first that
// fails.
func RunFix(f Fix, run func(argv []string) error) error {
	for _, s := range f.Steps {
		if err := run(s); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(s, " "), err)
		}
	}
	return nil
}

func isSelf(self, p config.Peer) bool {
	return strings.EqualFold(self.Host, p.Host) && self.User == p.User
}

// identityFiles lists the private keys ssh would offer to target, in order,
// as `ssh -G` reports them - so a key named in ~/.ssh/config counts, not just
// the id_* defaults. A variable so tests do not read the real ~/.ssh/config.
var identityFiles = func(target string) []string {
	out, err := exec.Command("ssh", "-G", target).Output()
	if err != nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.EqualFold(k, "identityfile") {
			continue
		}
		if rest, ok := strings.CutPrefix(v, "~/"); ok && home != "" {
			v = filepath.Join(home, rest)
		}
		files = append(files, v)
	}
	return files
}

// agentHasKeys reports whether ssh-agent holds a key, which plain
// ssh-copy-id would copy. A variable for the same reason as identityFiles.
var agentHasKeys = func() bool {
	return exec.Command("ssh-add", "-L").Run() == nil
}

// copyIDSteps installs this machine's key on target. ssh-copy-id itself
// ignores ~/.ssh/config: without -i it copies the agent's keys or the newest
// ~/.ssh/id*.pub - so for a key named by IdentityFile it would find nothing,
// or copy a key ssh never offers. Point it at the key ssh uses; generate one
// only when there really is none.
func copyIDSteps(target string) [][]string {
	for _, k := range identityFiles(target) {
		if _, err := os.Stat(k + ".pub"); err == nil {
			return [][]string{{"ssh-copy-id", "-i", k + ".pub", target}}
		}
	}
	if agentHasKeys() {
		return [][]string{{"ssh-copy-id", target}}
	}
	return [][]string{{"ssh-keygen", "-t", "ed25519"}, {"ssh-copy-id", target}}
}

// remoteCopyID is copyIDSteps as a POSIX script, for a peer whose files this
// machine cannot see.
func remoteCopyID(target string) string {
	t := shellQuote(target)
	return `k=; for f in $(ssh -G ` + t + ` 2>/dev/null | awk '$1=="identityfile"{print $2}'); do ` +
		`case $f in "~/"*) f="$HOME/${f#"~/"}";; esac; ` +
		`if [ -f "$f.pub" ]; then k="$f.pub"; break; fi; done; ` +
		`if [ -n "$k" ]; then ssh-copy-id -i "$k" ` + t + `; ` +
		`elif ssh-add -L >/dev/null 2>&1; then ssh-copy-id ` + t + `; ` +
		`else ssh-keygen -t ed25519 && ssh-copy-id ` + t + `; fi`
}

// shellQuote leaves plain words alone and single-quotes anything else, for
// display and for the remote script of a peer-to-peer fix.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"`$;&|<>(){}*?[]!#~\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
