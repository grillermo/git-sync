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

// withKey points HOME at a temp dir that does (or does not) hold a public key.
func withKey(t *testing.T, has bool) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if has {
		_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
		_ = os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("k"), 0o600)
	}
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

func TestAnUnknownHostKeyIsFixedByAcceptingItInteractively(t *testing.T) {
	withKey(t, true)
	fixes := FixesFor(fixSelf, []KeyResult{
		failed(fixSelf, mini, "ssh mini: exit status 255: Host key verification failed."),
	})
	if len(fixes) != 1 {
		t.Fatalf("fixes = %+v", fixes)
	}
	f := fixes[0]
	if want := []string{"ssh-copy-id g@Guillermos-Mac-mini.local"}; !reflect.DeepEqual(f.Commands(), want) {
		t.Errorf("commands = %q, want %q", f.Commands(), want)
	}
	if !strings.Contains(f.Explain, "host key") || f.Risky {
		t.Errorf("fix = %+v", f)
	}
}

func TestAFixGeneratesAKeyFirstWhenThereIsNone(t *testing.T) {
	withKey(t, false)
	f := FixesFor(fixSelf, []KeyResult{failed(fixSelf, mini, "Permission denied (publickey).")})[0]
	want := []string{"ssh-keygen -t ed25519", "ssh-copy-id g@Guillermos-Mac-mini.local"}
	if !reflect.DeepEqual(f.Commands(), want) {
		t.Errorf("commands = %q, want %q", f.Commands(), want)
	}
}

func TestAChangedHostKeyIsRiskyAndForgetsTheOldKey(t *testing.T) {
	withKey(t, true)
	f := FixesFor(fixSelf, []KeyResult{failed(fixSelf, mini, "REMOTE HOST IDENTIFICATION HAS CHANGED!")})[0]
	want := []string{"ssh-keygen -R Guillermos-Mac-mini.local", "ssh-copy-id g@Guillermos-Mac-mini.local"}
	if !f.Risky || !reflect.DeepEqual(f.Commands(), want) {
		t.Errorf("fix = %+v / %q", f, f.Commands())
	}
}

func TestAPeerToPeerFixRunsOnThePeerOverATerminal(t *testing.T) {
	f := FixesFor(fixSelf, []KeyResult{
		{From: fixSelf, To: mini, OK: true},
		failed(mini, old, "Host key verification failed."),
	})[0]
	if len(f.Steps) != 1 || !reflect.DeepEqual(f.Steps[0][:3], []string{"ssh", "-t", "g@Guillermos-Mac-mini.local"}) {
		t.Fatalf("steps = %q", f.Steps)
	}
	if script := f.Steps[0][3]; !strings.HasSuffix(script, "ssh-copy-id g@macbook-mid-2015.local") ||
		!strings.Contains(script, "ssh-keygen -t ed25519") {
		t.Errorf("remote script = %q", script)
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
