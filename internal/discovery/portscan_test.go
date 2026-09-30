package discovery

import (
	"context"
	"math"
	"net"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

// The enumeration tests are ported from vvterm's
// LocalSSHDiscoveryHostEnumerationTests, which pin the overflow edges.

func TestMaximumNetworkAndBroadcastProduceNoHosts(t *testing.T) {
	if got := hosts(math.MaxUint32, math.MaxUint32, math.MaxUint32); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestZeroNetworkEnumeratesOnlyAddressesBetweenNetworkAndBroadcast(t *testing.T) {
	if got := hosts(0, 3, 0); !reflect.DeepEqual(got, []string{"0.0.0.1", "0.0.0.2"}) {
		t.Errorf("got %v", got)
	}
}

func TestPointToPointAndSingleAddressRangesAreEmpty(t *testing.T) {
	if got := enumerateHosts(0xC000_0200, 0xFFFF_FFFE); len(got) != 0 {
		t.Errorf("/31: got %v, want none", got)
	}
	if got := enumerateHosts(0xC000_0201, math.MaxUint32); len(got) != 0 {
		t.Errorf("/32: got %v, want none", got)
	}
}

func TestOversizedDirectRangeIsRejected(t *testing.T) {
	if got := hosts(0, math.MaxUint32, 1); len(got) != 0 {
		t.Errorf("got %d hosts, want none", len(got))
	}
}

func TestCurrentAddressIsExcluded(t *testing.T) {
	got := hosts(0xC000_0200, 0xC000_0204, 0xC000_0202)
	if !reflect.DeepEqual(got, []string{"192.0.2.1", "192.0.2.3"}) {
		t.Errorf("got %v", got)
	}
}

func TestBroadSubnetEnumeratesOnlyTheCurrentSlash24(t *testing.T) {
	got := enumerateHosts(0x0A01_0203, 0xFFFF_0000)
	if len(got) != 253 || got[0] != "10.1.2.1" || got[len(got)-1] != "10.1.2.254" {
		t.Fatalf("got %d hosts from %s to %s", len(got), got[0], got[len(got)-1])
	}
	if slices.Contains(got, "10.1.2.3") || slices.Contains(got, "10.1.3.1") {
		t.Error("must skip this machine and stay inside its /24")
	}
}

func TestMaximumBroadcastEnumeratesItsSlash24(t *testing.T) {
	got := enumerateHosts(math.MaxUint32, 0xFFFF_FF00)
	if len(got) != 254 || got[0] != "255.255.255.1" || got[len(got)-1] != "255.255.255.254" {
		t.Errorf("got %d hosts from %s to %s", len(got), got[0], got[len(got)-1])
	}
}

func TestSweepReportsOnlyListeningHosts(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// A closed port on the same address: connection refused, not a hit.
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	var mu sync.Mutex
	var found []Host
	collect := func(h Host) { mu.Lock(); found = append(found, h); mu.Unlock() }

	sweep(context.Background(), []string{"127.0.0.1"}, port, time.Second, 4, collect)
	sweep(context.Background(), []string{"127.0.0.1"}, closedPort, time.Second, 4, collect)

	if len(found) != 1 {
		t.Fatalf("found %+v, want exactly the listening host", found)
	}
	h := found[0]
	if h.Host != "127.0.0.1" || !reflect.DeepEqual(h.Addrs, []string{"127.0.0.1"}) ||
		!reflect.DeepEqual(h.Sources, []Source{SourcePortScan}) || h.Latency <= 0 {
		t.Errorf("hit = %+v", h)
	}
}

func TestSweepStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	sweep(ctx, []string{"127.0.0.1", "127.0.0.2"}, 1, time.Second, 1, func(Host) { called = true })
	if called {
		t.Error("a cancelled sweep must not report anything")
	}
}
