package setup

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
)

var (
	fixSelf = config.Peer{Host: "laptop.local", User: "g"}
	mini    = config.Peer{Host: "Guillermos-Mac-mini.local", User: "g"}
	old     = config.Peer{Host: "macbook-mid-2015.local", User: "g"}
)

func failed(from, to config.Peer, out string) KeyResult {
	return KeyResult{From: from, To: to, Err: firstLine(out), Output: out}
}

// withKeys stands in for what ssh would offer: the identity files `ssh -G`
// lists (created on disk, with a .pub for those in withPub) and whether the
// agent holds a key.
func withKeys(t *testing.T, agent bool, files map[string]bool) {
	dir := t.TempDir()
	var paths []string
	for name, withPub := range files {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte("private"), 0o600)
		if withPub {
			_ = os.WriteFile(p+".pub", []byte("public"), 0o600)
		}
		paths = append(paths, p)
	}
	origFiles, origAgent := identityFiles, agentHasKeys
	identityFiles = func(string) []string { return paths }
	agentHasKeys = func() bool { return agent }
	t.Cleanup(func() { identityFiles, agentHasKeys = origFiles, origAgent })
}

func TestDiagnoseTellsTheFailuresApart(t *testing.T) {
	for out, want := range map[string]Problem{
		"Host key verification failed.": ProblemHostKeyUnknown,
		"No ED25519 host key is known for mini and you have requested strict checking.\r\nHost key verification failed.": ProblemHostKeyUnknown,
		"@@@@\r\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\r\nHost key verification failed.":           ProblemHostKeyChanged,
		"g@mini: Permission denied (publickey,password).":                                                                ProblemAuth,
		"ssh: Could not resolve hostname mini.local: nodename nor servname provided, or not known":                       ProblemUnresolvable,
		"ssh: connect to host mini port 22: Connection refused":                                                          ProblemRefused,
		"ssh: connect to host mini port 22: Operation timed out":                                                         ProblemOffline,
		"kex_exchange_identification: read: Connection reset by peer":                                                    ProblemOther,
	} {
		if got := diagnose(out); got != want {
			t.Errorf("diagnose(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestAnUnknownHostKeyIsFixedByConnectingOnceByHand(t *testing.T) {
	withKeys(t, false, nil)
	fixes := FixesFor(fixSelf, []KeyResult{
		failed(fixSelf, mini, "ssh mini: exit status 255: Host key verification failed."),
	})
	if len(fixes) != 1 {
		t.Fatalf("fixes = %+v", fixes)
	}
	f := fixes[0]
	// Not ssh-keygen/ssh-copy-id: login is not what failed.
	if want := []string{"ssh g@Guillermos-Mac-mini.local true"}; !reflect.DeepEqual(f.Commands(), want) {
		t.Errorf("commands = %q, want %q", f.Commands(), want)
	}
	if !strings.Contains(f.Explain, "host key") || f.Risky {
		t.Errorf("fix = %+v", f)
	}
}

func TestAChangedHostKeyIsRiskyAndForgetsTheOldKey(t *testing.T) {
	f := FixesFor(fixSelf, []KeyResult{failed(fixSelf, mini, "REMOTE HOST IDENTIFICATION HAS CHANGED!")})[0]
	want := []string{"ssh-keygen -R Guillermos-Mac-mini.local", "ssh g@Guillermos-Mac-mini.local true"}
	if !f.Risky || !reflect.DeepEqual(f.Commands(), want) {
		t.Errorf("fix = %+v / %q", f, f.Commands())
	}
}

func TestARejectedKeyCopiesTheKeySshActuallyOffers(t *testing.T) {
	// A key named by IdentityFile in ~/.ssh/config, not an id_* default:
	// plain ssh-copy-id would never find it.
	withKeys(t, false, map[string]bool{"macbook_pro_2022": true})
	f := FixesFor(fixSelf, []KeyResult{failed(fixSelf, mini, "Permission denied (publickey).")})[0]
	if len(f.Steps) != 1 {
		t.Fatalf("steps = %q", f.Steps)
	}
	s := f.Steps[0]
	if len(s) != 4 || s[0] != "ssh-copy-id" || s[1] != "-i" ||
		filepath.Base(s[2]) != "macbook_pro_2022.pub" || s[3] != "g@Guillermos-Mac-mini.local" {
		t.Errorf("steps = %q", f.Steps)
	}
}

func TestARejectedKeyFallsBackToTheAgent(t *testing.T) {
	withKeys(t, true, map[string]bool{"id_ed25519": false})
	f := FixesFor(fixSelf, []KeyResult{failed(fixSelf, mini, "Permission denied (publickey).")})[0]
	if want := []string{"ssh-copy-id g@Guillermos-Mac-mini.local"}; !reflect.DeepEqual(f.Commands(), want) {
		t.Errorf("commands = %q, want %q", f.Commands(), want)
	}
}

func TestAFixGeneratesAKeyOnlyWhenThereIsNone(t *testing.T) {
	withKeys(t, false, nil)
	f := FixesFor(fixSelf, []KeyResult{failed(fixSelf, mini, "Permission denied (publickey).")})[0]
	want := []string{"ssh-keygen -t ed25519", "ssh-copy-id g@Guillermos-Mac-mini.local"}
	if !reflect.DeepEqual(f.Commands(), want) {
		t.Errorf("commands = %q, want %q", f.Commands(), want)
	}
}

func TestAPeerToPeerHostKeyFixRunsOnThePeerOverATerminal(t *testing.T) {
	f := FixesFor(fixSelf, []KeyResult{
		{From: fixSelf, To: mini, OK: true},
		failed(mini, old, "Host key verification failed."),
	})[0]
	want := [][]string{{"ssh", "-t", "g@Guillermos-Mac-mini.local", "ssh g@macbook-mid-2015.local true"}}
	if !reflect.DeepEqual(f.Steps, want) {
		t.Errorf("steps = %q, want %q", f.Steps, want)
	}
}

func TestAPeerToPeerKeyFixFindsThePeersOwnKey(t *testing.T) {
	f := FixesFor(fixSelf, []KeyResult{
		{From: fixSelf, To: mini, OK: true},
		failed(mini, old, "Permission denied (publickey)."),
	})[0]
	if len(f.Steps) != 1 || !reflect.DeepEqual(f.Steps[0][:3], []string{"ssh", "-t", "g@Guillermos-Mac-mini.local"}) {
		t.Fatalf("steps = %q", f.Steps)
	}
	script := f.Steps[0][3]
	for _, want := range []string{"ssh -G g@macbook-mid-2015.local", `ssh-copy-id -i "$k" g@macbook-mid-2015.local`,
		"ssh-keygen -t ed25519 && ssh-copy-id g@macbook-mid-2015.local"} {
		if !strings.Contains(script, want) {
			t.Errorf("remote script lacks %q:\n%s", want, script)
		}
	}
}

func TestAPairOnAnUnreachablePeerSaysToFixThatFirst(t *testing.T) {
	fixes := FixesFor(fixSelf, []KeyResult{
		failed(fixSelf, mini, "Host key verification failed."),
		failed(mini, old, "Host key verification failed."),
	})
	if len(fixes) != 2 || len(fixes[1].Steps) != 0 || !strings.Contains(fixes[1].Hint, "first") {
		t.Errorf("fixes = %+v", fixes)
	}
}

func TestSshdOffHasNoCommandOnlyAHint(t *testing.T) {
	f := FixesFor(fixSelf, []KeyResult{failed(fixSelf, old, "port 22: Connection refused")})[0]
	if len(f.Steps) != 0 || !strings.Contains(f.Hint, "Remote Login") {
		t.Errorf("fix = %+v", f)
	}
}

func TestRunFixStopsAtTheFirstFailure(t *testing.T) {
	var ran []string
	err := RunFix(Fix{Steps: [][]string{{"a"}, {"b"}, {"c"}}}, func(argv []string) error {
		ran = append(ran, argv[0])
		if argv[0] == "b" {
			return errors.New("boom")
		}
		return nil
	})
	if err == nil || !reflect.DeepEqual(ran, []string{"a", "b"}) {
		t.Errorf("ran %v, err %v", ran, err)
	}
}
