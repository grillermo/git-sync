package discovery

import (
	"reflect"
	"testing"
	"time"
)

func bonjour(name, host string, addrs ...string) Host {
	return Host{Name: name, Host: host, Addrs: addrs, Sources: []Source{SourceBonjour}}
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
	want := Host{Name: "Studio", Host: "studio.local", Addrs: []string{"192.168.1.5"},
		Sources: []Source{SourceBonjour, SourcePortScan}, Latency: 3 * time.Millisecond}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

func TestSetUpgradesAnEarlierPortScanHitToItsBonjourName(t *testing.T) {
	// The sweep can reach an address before mDNS names it.
	var s Set
	s.Add(scanned("192.168.1.5", 3*time.Millisecond))
	s.Add(bonjour("Studio", "studio.local", "192.168.1.5"))

	got := s.List()
	if len(got) != 1 || got[0].Host != "studio.local" || got[0].Name != "Studio" ||
		got[0].Latency != 3*time.Millisecond {
		t.Errorf("got %+v, want one studio.local entry keeping the latency", got)
	}
}

func TestSetAbsorbsAnAddressLearnedLater(t *testing.T) {
	// The first Bonjour answer had no A record; the port-scan hit is its own
	// row until a later answer says whose address it is.
	var s Set
	s.Add(bonjour("Studio", "studio.local"))
	s.Add(scanned("192.168.1.5", time.Millisecond))
	if len(s.List()) != 2 {
		t.Fatalf("got %+v, want two rows before the address is known", s.List())
	}
	s.Add(bonjour("Studio", "studio.local", "192.168.1.5"))
	if got := s.List(); len(got) != 1 || len(got[0].Sources) != 2 {
		t.Errorf("got %+v, want the hit folded into studio.local", got)
	}
}

func TestSetReportsWhetherAnythingChanged(t *testing.T) {
	var s Set
	if !s.Add(bonjour("Studio", "studio.local")) {
		t.Error("a new host is a change")
	}
	if s.Add(bonjour("Studio", "studio.local")) {
		t.Error("the same answer again (e.g. from _sftp-ssh) is not a change")
	}
	if s.Add(Host{}) {
		t.Error("a host with no address is ignored")
	}
}

func TestSetSortsBonjourFirstThenByName(t *testing.T) {
	var s Set
	s.Add(scanned("10.0.0.2", time.Millisecond))
	s.Add(bonjour("zeta", "zeta.local"))
	s.Add(bonjour("Alpha", "alpha.local"))

	var order []string
	for _, h := range s.List() {
		order = append(order, h.Host)
	}
	if want := []string{"alpha.local", "zeta.local", "10.0.0.2"}; !reflect.DeepEqual(order, want) {
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
		bonjour("Studio", "Studio.local."),
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
