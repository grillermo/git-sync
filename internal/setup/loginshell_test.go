package setup_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

// ssh runs a remote command through the peer's login shell, which on macOS
// is zsh - and zsh does not word-split an unquoted $var. A repo with two
// remotes, neither of them the first preference, must still resolve.
func TestRemoteScriptsResolveTheRemoteUnderZsh(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("no zsh on this machine")
	}
	sb := testutil.NewSandbox(t)
	path := sb.MakeRepo("two-remotes")
	sb.AddRemote(t, path, "upstream", "two-remotes")
	base := filepath.Dir(path)
	prefs := []string{"github", "origin"}

	for name, script := range map[string]string{
		"repocheck":   setup.RepoCheckScript(base, []string{"two-remotes"}, prefs),
		"initialsync": setup.SyncMeasureScript(base, []string{"two-remotes"}, prefs),
	} {
		out, err := exec.Command(zsh, "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: zsh: %v\n%s", name, err, out)
		}
		if strings.Contains(string(out), "no-remote") || strings.Contains(string(out), "no remote") {
			t.Errorf("%s under zsh found no remote:\n%s", name, out)
		}
	}
}
