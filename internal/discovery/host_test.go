package discovery

import (
	"reflect"
	"testing"
	"time"
)

// bonjour is a host as mdnsCache.take reports it: the ssh target is one of
// its addresses, the .local name only rides along.
func bonjour(name, hostname string, addrs ...string) Host {
	return Host{Name: name, Host: bestAddr(addrs), Hostname: hostname, Addrs: addrs, Sources: []Source{SourceBonjour}}
}

func scanned(ip string, latency time.Duration) Host {
	return Host{Name: ip, Host: ip, Addrs: []string{ip}, Sources: []Source{SourcePortScan}, Latency: latency}
}

func TestSetMergesAPortScanHitIntoTheBonjourHostThatOwnsTheAddress(t *testing.T) {
	var s Set
	s.Add(bonjour("Studio", "studio.local", "192.168.1.5"))
	s.Add(scanned("192.168.1.5", 3*time.Millisecond))

	got := s.List()
	if len(got) != 1 {
		t.Fatalf("got %d hosts, want 1: %+v", len(got), got)
	}
	want := Host{Name: "Studio", Host: "192.168.1.5", Hostname: "studio.local", Addrs: []string{"192.168.1.5"},
		Sources: []Source{SourceBonjour, SourcePortScan}, Latency: 3 * time.Millisecond}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

func TestSetNamesAnEarlierPortScanHitButKeepsSshingToTheIP(t *testing.T) {
	// The sweep can reach an address before mDNS names it.
	var s Set
	s.Add(scanned("192.168.1.5", 3*time.Millisecond))
	s.Add(bonjour("Studio", "studio.local", "192.168.1.5"))

	got := s.List()
	if len(got) != 1 || got[0].Host != "192.168.1.5" || got[0].Hostname != "studio.local" ||
		got[0].Name != "Studio" || got[0].Latency != 3*time.Millisecond {
		t.Errorf("got %+v, want one Studio entry at 192.168.1.5 keeping the latency", got)
	}
}

func TestSetSshesToTheAddressThatAnsweredTheSweep(t *testing.T) {
	// Bonjour advertises two addresses; only the sweep says which one
	// actually answers ssh from here.
	var s Set
	s.Add(bonjour("Studio", "studio.local", "10.0.0.9", "192.168.1.5"))
	s.Add(scanned("192.168.1.5", time.Millisecond))
	if got := s.List(); len(got) != 1 || got[0].Host != "192.168.1.5" {
		t.Errorf("got %+v, want the swept 192.168.1.5", got)
	}
	// A second hit on the other address does not move it again.
	s.Add(scanned("10.0.0.9", time.Millisecond))
	if got := s.List(); len(got) != 1 || got[0].Host != "192.168.1.5" {
		t.Errorf("got %+v, want 192.168.1.5 to stick", got)
	}
}

func TestSetAbsorbsAnAddressLearnedLater(t *testing.T) {
	// The first Bonjour answer knew one address; the port-scan hit on the
	// other is its own row until a later answer says whose address it is.
	var s Set
	s.Add(bonjour("Studio", "studio.local", "10.0.0.9"))
	s.Add(scanned("192.168.1.5", time.Millisecond))
	if len(s.List()) != 2 {
		t.Fatalf("got %+v, want two rows before the address is known", s.List())
	}
	s.Add(bonjour("Studio", "studio.local", "10.0.0.9", "192.168.1.5"))
	if got := s.List(); len(got) != 1 || len(got[0].Sources) != 2 || got[0].Host != "192.168.1.5" {
		t.Errorf("got %+v, want the hit folded into Studio, sshing to it", got)
	}
}

func TestBestAddrSkipsLinkLocal(t *testing.T) {
	if got := bestAddr([]string{"169.254.3.4", "192.168.1.5"}); got != "192.168.1.5" {
		t.Errorf("bestAddr = %q", got)
	}
	if got := bestAddr([]string{"169.254.3.4"}); got != "169.254.3.4" {
		t.Errorf("bestAddr = %q, want the only address", got)
	}
}

func TestSetReportsWhetherAnythingChanged(t *testing.T) {
	var s Set
	if !s.Add(bonjour("Studio", "studio.local", "192.168.1.5")) {
		t.Error("a new host is a change")
	}
	if s.Add(bonjour("Studio", "studio.local", "192.168.1.5")) {
		t.Error("the same answer again (e.g. from _sftp-ssh) is not a change")
	}
	if s.Add(Host{}) {
		t.Error("a host with no address is ignored")
	}
}

func TestSetSortsBonjourFirstThenByName(t *testing.T) {
	var s Set
	s.Add(scanned("10.0.0.2", time.Millisecond))
	s.Add(bonjour("zeta", "zeta.local", "10.0.0.8"))
	s.Add(bonjour("Alpha", "alpha.local", "10.0.0.7"))

	var order []string
	for _, h := range s.List() {
		order = append(order, h.Host)
	}
	if want := []string{"10.0.0.7", "10.0.0.8", "10.0.0.2"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestSetIsCapped(t *testing.T) {
	var s Set
	for i := 0; i < maxHosts+10; i++ {
		s.Add(scanned(ipv4String(uint32(0x0A000000+i)), time.Millisecond))
	}
	if n := len(s.List()); n != maxHosts {
		t.Errorf("len = %d, want %d", n, maxHosts)
	}
}

func TestIdentityRecognisesThisMachine(t *testing.T) {
	id := identity{names: map[string]bool{"studio": true}, addrs: map[string]bool{"192.168.1.5": true}}
	for _, h := range []Host{
		bonjour("Studio", "Studio.local.", "10.0.0.3"),
		bonjour("x", "other.local", "192.168.1.5"),
		scanned("192.168.1.5", time.Millisecond),
	} {
		if !id.is(h) {
			t.Errorf("%+v should be recognised as this machine", h)
		}
	}
	if id.is(bonjour("Laptop", "laptop.local", "192.168.1.6")) {
		t.Error("another machine is not this one")
	}
}
