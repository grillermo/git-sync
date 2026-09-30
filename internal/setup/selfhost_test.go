package setup

import (
	"net"
	"testing"

	"github.com/grillermo/git-sync/internal/discovery"
)

func TestSelfHostPrefersTheFlagThenTheLANAddress(t *testing.T) {
	if got := SelfHost("mini.example"); got != "mini.example" {
		t.Errorf("SelfHost(flag) = %q", got)
	}
	if _, ok := discovery.LocalIP(); !ok {
		t.Skip("no IPv4 interface here")
	}
	if got := SelfHost(""); net.ParseIP(got) == nil {
		t.Errorf("SelfHost default = %q, want this machine's IP, not its hostname", got)
	}
}
