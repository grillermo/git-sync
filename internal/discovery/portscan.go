package discovery

import (
	"context"
	"encoding/binary"
	"math/bits"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port-scan tuning, as in LocalSSHDiscoveryService: a short connect timeout
// keeps a full /24 inside the scan budget at this concurrency.
const (
	portScanTimeout     = 350 * time.Millisecond
	portScanConcurrency = 24
	sshPort             = 22
)

// maxCandidates bounds the sweep to a single /24's worth of hosts, whatever
// the real netmask says.
const maxCandidates = 254

// localSubnet picks the interface to sweep: an up, non-loopback IPv4, keeping
// the first en* (the built-in Ethernet/Wi-Fi on macOS, and the systemd
// predictable name on Linux) and otherwise whichever came last.
func localSubnet() (addr, mask uint32, ok bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, 0, false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, isNet := a.(*net.IPNet)
			if !isNet {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || len(ipn.Mask) != net.IPv4len {
				continue
			}
			a32, m32 := binary.BigEndian.Uint32(ip4), binary.BigEndian.Uint32(ipn.Mask)
			if a32 == 0 || m32 == 0 {
				continue
			}
			addr, mask, ok = a32, m32, true
			if strings.HasPrefix(ifc.Name, "en") {
				return addr, mask, ok
			}
		}
	}
	return addr, mask, ok
}

// enumerateHosts lists the addresses to probe around address. Anything
// broader than a /24 is narrowed to the /24 this machine sits in - sweeping a
// /16 would take minutes and mostly find nothing.
func enumerateHosts(address, netmask uint32) []string {
	if bits.OnesCount32(netmask) < 24 {
		slice := address & 0xFFFFFF00
		return hosts(slice, slice|0x000000FF, address)
	}
	network := address & netmask
	return hosts(network, network|^netmask, address)
}

// hosts lists every address strictly between network and broadcast except
// current, or nothing if that range is empty or larger than maxCandidates.
func hosts(network, broadcast, current uint32) []string {
	if network >= broadcast || broadcast-network < 2 {
		return nil
	}
	start, end := network+1, broadcast-1
	if uint64(end)-uint64(start)+1 > maxCandidates {
		return nil
	}
	out := make([]string, 0, end-start+1)
	for a := uint64(start); a <= uint64(end); a++ {
		if uint32(a) != current {
			out = append(out, ipv4String(uint32(a)))
		}
	}
	return out
}

func ipv4String(a uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], a)
	return net.IP(b[:]).String()
}

// sweep connects to port on every candidate, at most concurrency at a time,
// reporting each one that accepts. It returns once every probe has finished
// or ctx is done.
func sweep(ctx context.Context, candidates []string, port int, timeout time.Duration,
	concurrency int, found func(Host)) {
	sem := make(chan struct{}, max(1, concurrency))
	var wg sync.WaitGroup
	for _, ip := range candidates {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if latency, ok := probe(ctx, ip, port, timeout); ok {
				found(Host{
					Name: ip, Host: ip, Addrs: []string{ip},
					Sources: []Source{SourcePortScan}, Latency: latency,
				})
			}
		}()
	}
	wg.Wait()
}

// probe reports whether ip accepts a TCP connection on port within timeout.
func probe(ctx context.Context, ip string, port int, timeout time.Duration) (time.Duration, bool) {
	d := net.Dialer{Timeout: timeout}
	started := time.Now()
	conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0, false
	}
	_ = conn.Close()
	return max(time.Millisecond, time.Since(started)), true
}
