package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestHomeDefaultsToDotGitsync(t *testing.T) {
	t.Setenv("HOME", "/tmp/fakehome")
	t.Setenv("GITSYNC_HOME", "")
	if got, want := config.Home(), "/tmp/fakehome/.gitsync"; got != want {
		t.Errorf("Home() = %q, want %q", got, want)
	}
}

func TestHomeHonoursEnvOverride(t *testing.T) {
	t.Setenv("GITSYNC_HOME", "/tmp/elsewhere")
	if got, want := config.Home(), "/tmp/elsewhere"; got != want {
		t.Errorf("Home() = %q, want %q", got, want)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	sb := testutil.NewSandbox(t)
	// Save with old form, which Marshal() will convert to new form
	want := config.Config{BaseDir: sb.BaseDir, PeerHost: "peer.example", PeerUser: "tester"}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Compare field by field: Config now holds a slice, so it is not
	// comparable with ==. Marshal converts old form to new form, so we
	// expect Peers to be populated and old fields to be empty.
	if got.BaseDir != want.BaseDir {
		t.Errorf("Load() BaseDir = %q, want %q", got.BaseDir, want.BaseDir)
	}
	if len(got.Peers) != 1 || got.Peers[0].Host != "peer.example" || got.Peers[0].User != "tester" {
		t.Errorf("Load() Peers = %v, want one peer tester@peer.example", got.Peers)
	}
}

func TestSaveWritesReadableToml(t *testing.T) {
	sb := testutil.NewSandbox(t)
	c := config.Config{BaseDir: sb.BaseDir, PeerHost: "peer.example", PeerUser: "tester"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(sb.GitsyncHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// It is hand-editable; keep the keys snake_case and obvious.
	// Old peer_host/peer_user form is never written; only the new peers format.
	for _, want := range []string{"base_dir"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config.toml missing key %q:\n%s", want, b)
		}
	}
	// Should have peers section with host and user keys
	if !strings.Contains(string(b), "[[peers]]") && !strings.Contains(string(b), "host") {
		t.Errorf("config.toml should have peers section:\n%s", b)
	}
}

func TestLoadMissingConfigIsDistinguishable(t *testing.T) {
	testutil.NewSandbox(t)
	_, err := config.Load()
	if !os.IsNotExist(err) && err == nil {
		t.Fatal("Load on a missing config should return an error")
	}
	if !config.IsNotInstalled(err) {
		t.Errorf("IsNotInstalled(%v) = false, want true", err)
	}
}

func TestRepoRelHappyPath(t *testing.T) {
	c := config.Config{BaseDir: "/home/me/code"}
	got, err := c.RepoRel("/home/me/code/group/proj")
	if err != nil {
		t.Fatalf("RepoRel: %v", err)
	}
	if want := "group/proj"; got != want {
		t.Errorf("RepoRel = %q, want %q", got, want)
	}
}

func TestRepoRelRejectsOutsideBaseDir(t *testing.T) {
	c := config.Config{BaseDir: "/home/me/code"}
	for _, in := range []string{"/home/me/other/proj", "/home/me", "/home/me/codex/proj", "/"} {
		if _, err := c.RepoRel(in); err == nil {
			t.Errorf("RepoRel(%q) succeeded, want an error", in)
		}
	}
}

func TestRepoRelRejectsBaseDirItself(t *testing.T) {
	c := config.Config{BaseDir: "/home/me/code"}
	if _, err := c.RepoRel("/home/me/code"); err == nil {
		t.Error("RepoRel(base_dir) succeeded, want an error")
	}
}

func TestRepoPathIsTheInverseOfRepoRel(t *testing.T) {
	c := config.Config{BaseDir: "/home/me/code"}
	if got, want := c.RepoPath("group/proj"), "/home/me/code/group/proj"; got != want {
		t.Errorf("RepoPath = %q, want %q", got, want)
	}
}

func TestIsSelected(t *testing.T) {
	c := config.Config{Repos: []string{"work/api", "notes"}}
	for _, in := range []string{"work/api", "notes", "work/api/"} {
		if !c.IsSelected(in) {
			t.Errorf("IsSelected(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"work", "work/api/sub", "other", "", "api"} {
		if c.IsSelected(in) {
			t.Errorf("IsSelected(%q) = true, want false", in)
		}
	}
}

func TestIsSelectedOnAnEmptyAllowlist(t *testing.T) {
	// A fresh config syncs nothing. Sync is opt-in; an empty list must not
	// be read as "everything".
	c := config.Config{}
	if c.IsSelected("anything") {
		t.Error("an empty allowlist must select nothing")
	}
}

func TestSaveThenLoadRoundTripsRepos(t *testing.T) {
	testutil.NewSandbox(t)
	want := []string{"notes", "work/api", "work/web"}
	c := config.Config{BaseDir: "/tmp/x", PeerHost: "p", PeerUser: "u", Repos: want}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != len(want) {
		t.Fatalf("Repos = %v, want %v", got.Repos, want)
	}
	for i := range want {
		if got.Repos[i] != want[i] {
			t.Errorf("Repos[%d] = %q, want %q", i, got.Repos[i], want[i])
		}
	}
}

func TestRemotesDefaultsToGithubThenOrigin(t *testing.T) {
	// The remote is the transport, so this default is load-bearing: get it
	// wrong and every repo whose remote is called github stops syncing.
	c := config.Config{}
	got := c.Remotes()
	if len(got) != 2 || got[0] != "github" || got[1] != "origin" {
		t.Errorf("Remotes() = %v, want [github origin]", got)
	}
}

func TestRemotesHonoursAnExplicitList(t *testing.T) {
	c := config.Config{RemoteNames: []string{"upstream"}}
	if got := c.Remotes(); len(got) != 1 || got[0] != "upstream" {
		t.Errorf("Remotes() = %v, want [upstream]", got)
	}
}

func TestSaveWritesRemoteNames(t *testing.T) {
	// It is hand-editable; someone whose remote is called neither github nor
	// origin needs to see the knob in the file.
	testutil.NewSandbox(t)
	c := config.Config{BaseDir: "/tmp/x", PeerHost: "p", PeerUser: "u"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(config.Path())
	if !strings.Contains(string(b), "remote_names") {
		t.Errorf("config.toml should record remote_names:\n%s", b)
	}
}

func TestRepoPathRejectsEscapingRelpaths(t *testing.T) {
	// A relpath arrives over ssh from the peer. Never let it escape base_dir.
	c := config.Config{BaseDir: "/home/me/code"}
	for _, in := range []string{"../etc", "a/../../etc", "/etc", "."} {
		if err := c.ValidateRel(in); err == nil {
			t.Errorf("ValidateRel(%q) = nil, want an error", in)
		}
	}
}

func TestLoadMigratesTheOldSinglePeerForm(t *testing.T) {
	sb := testutil.NewSandbox(t)
	writeConfig(t, sb, `base_dir = "`+sb.BaseDir+`"
peer_host = "old.local"
peer_user = "tester"
repos = ["a"]
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	peers := cfg.PeerList()
	if len(peers) != 1 || peers[0].Host != "old.local" || peers[0].User != "tester" {
		t.Fatalf("PeerList() = %+v, want one peer tester@old.local", peers)
	}
}

func TestPeerListDropsDuplicatesAndSelf(t *testing.T) {
	self, _ := os.Hostname()
	cfg := config.Config{Peers: []config.Peer{
		{Host: "b.local", User: "t"},
		{Host: "b.local", User: "t"},
		{Host: self, User: "t"},
	}}
	peers := cfg.PeerList()
	if len(peers) != 1 || peers[0].Host != "b.local" {
		t.Fatalf("PeerList() = %+v, want only b.local", peers)
	}
}

func TestPeerValidateRejectsShellMetacharacters(t *testing.T) {
	for _, p := range []config.Peer{
		{Host: "a.local'; rm -rf ~", User: "t"},
		{Host: "a.local", User: "t;evil"},
		{Host: "a.local", User: "t", BaseDir: "/home/t'/x"},
		{Host: "", User: "t"},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", p)
		}
	}
	if err := (config.Peer{Host: "a.local", User: "t", BaseDir: "/home/t/code"}).Validate(); err != nil {
		t.Errorf("Validate on a plain peer: %v", err)
	}
}

func TestMarshalRoundTripsSeveralPeers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg := config.Config{BaseDir: sb.BaseDir, Repos: []string{"a"}, Peers: []config.Peer{
		{Host: "b.local", User: "t", BaseDir: "/home/t/code"},
		{Host: "c.local", User: "t", BaseDir: "/Users/t/code"},
	}}
	b, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "peer_host") {
		t.Errorf("Marshal still writes the old peer_host key:\n%s", b)
	}
	var got config.Config
	if _, err := toml.Decode(string(b), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Peers) != 2 || got.Peers[1].Host != "c.local" {
		t.Fatalf("round-trip lost peers: %+v", got.Peers)
	}
}

func writeConfig(t *testing.T, sb *testutil.Sandbox, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sb.GitsyncHome, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
