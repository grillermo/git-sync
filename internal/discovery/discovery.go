package discovery

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// ScanDuration is how long a scan runs, as in vvterm: long enough for a
// sleeping Mac's mDNS responder to answer a resent query, and for a full /24
// sweep at portScanConcurrency.
const ScanDuration = 6 * time.Second

// bonjourShare is how much of the scan the Bonjour browse gets, leaving room
// to report unresolved services before the scan's context is cancelled.
const bonjourShare = ScanDuration - 500*time.Millisecond

// Scan looks for ssh hosts on the local network for ScanDuration (or until ctx
// is done), sending each one on the returned channel as it is found - the
// same host may arrive more than once as more is learned about it; Set merges
// those. The channel closes when the scan ends. This machine itself is never
// reported.
func Scan(ctx context.Context) <-chan Host {
	ctx, cancel := context.WithTimeout(ctx, ScanDuration)
	out := make(chan Host)
	self := localIdentity()

	send := func(h Host) {
		if self.is(h) {
			return
		}
		select {
		case out <- h:
		case <-ctx.Done():
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = browse(ctx, bonjourShare, send)
	}()
	go func() {
		defer wg.Done()
		addr, mask, ok := localSubnet()
		if !ok {
			return
		}
		sweep(ctx, enumerateHosts(addr, mask), sshPort, portScanTimeout, portScanConcurrency, send)
	}()
	go func() {
		wg.Wait()
		cancel()
		close(out)
	}()
	return out
}

// identity is what this machine answers to, so a scan does not offer it as
// its own peer: Bonjour happily reports our own sshd advert back to us.
type identity struct {
	names map[string]bool
	addrs map[string]bool
}

func localIdentity() identity {
	id := identity{names: map[string]bool{}, addrs: map[string]bool{}}
	if h, err := os.Hostname(); err == nil {
		id.names[bareHost(h)] = true
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				id.addrs[ipn.IP.String()] = true
			}
		}
	}
	return id
}

func (id identity) is(h Host) bool {
	if id.names[bareHost(h.Host)] || id.addrs[h.Host] {
		return true
	}
	for _, a := range h.Addrs {
		if id.addrs[a] {
			return true
		}
	}
	return false
}

// bareHost lowercases a hostname and drops a trailing .local, so "Studio",
// "studio.local" and "studio.local." all compare equal.
func bareHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	return strings.TrimSuffix(h, ".local")
}
