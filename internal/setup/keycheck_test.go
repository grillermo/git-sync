package setup_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestCheckKeysProbesEveryOrderedPair(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := config.Peer{Host: "a.local", User: "t"}
	peers := []config.Peer{{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"}}

	results := setup.CheckKeys(self, peers)
	// a->b, a->c, b->c, c->b: four ordered pairs, and never a self-pair.
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4: %+v", len(results), results)
	}
	for _, r := range results {
		if r.From.Host == r.To.Host {
			t.Errorf("a machine was checked against itself: %+v", r)
		}
		if !r.OK {
			t.Errorf("all pairs should pass with a working ssh: %+v", r)
		}
	}
}

func TestCheckKeysReportsAFailingPair(t *testing.T) {
	sb := testutil.NewSandbox(t)
	// The outer ssh succeeds; the inner one (run on b, targeting c) fails.
	sb.StubSSHScripted(map[string]string{"*c.local*": "exit 255"}, 0)
	self := config.Peer{Host: "a.local", User: "t"}
	peers := []config.Peer{{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"}}

	results := setup.CheckKeys(self, peers)
	var failed []string
	for _, r := range results {
		if !r.OK {
			failed = append(failed, r.From.Host+"->"+r.To.Host)
		}
	}
	if len(failed) == 0 {
		t.Fatal("a failing target must produce a failing pair")
	}

	var out bytes.Buffer
	n := setup.RenderKeyChecks(&out, results)
	if n != len(failed) {
		t.Errorf("RenderKeyChecks = %d, want %d", n, len(failed))
	}
	if !strings.Contains(out.String(), "ssh-copy-id") {
		t.Errorf("the report must say how to fix it:\n%s", out.String())
	}
}
