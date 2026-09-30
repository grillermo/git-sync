// Package discovery finds machines on the local network that answer ssh, so
// install can offer them as peers instead of making the user type hostnames.
//
// Two sources run side by side, as in vvterm's LocalSSHDiscoveryService: a
// Bonjour (mDNS) browse for _ssh._tcp / _sftp-ssh._tcp, which names machines
// the way they name themselves, and a port-22 sweep of this machine's /24,
// which catches the ones that do not advertise. Discovery only ever suggests:
// nothing found here is trusted until install's own key-only reachability
// check has sshed to it.
package discovery

import (
	"slices"
	"strings"
	"time"
)

// Source is how a host was found.
type Source string

const (
	SourceBonjour  Source = "bonjour"
	SourcePortScan Source = "port 22"
)

// Host is one machine that looks like it answers ssh.
type Host struct {
	// Name is what the machine calls itself (the Bonjour instance name), or
	// the address when all we have is a port-scan hit.
	Name string
	// Host is what to ssh to: a .local name from Bonjour, else an IPv4.
	Host string
	// Addrs are the IPv4s known to belong to Host. A port-scan hit whose IP
	// is in here is the same machine, not a second one.
	Addrs   []string
	Sources []Source
	// Latency is the port-22 connect time; zero when only Bonjour saw it.
	Latency time.Duration
}

// Bonjour reports whether mDNS named this host - the stronger signal, since
// it is a name that survives a DHCP lease changing.
func (h Host) Bonjour() bool { return slices.Contains(h.Sources, SourceBonjour) }

// merge folds newer into h, mirroring DiscoveredSSHHost.merge: a real name
// beats a bare address, sources and addresses union, and the latest latency
// wins.
func (h *Host) merge(newer Host) {
	if newer.Name != "" && newer.Name != newer.Host {
		h.Name = newer.Name
	}
	for _, s := range newer.Sources {
		if !slices.Contains(h.Sources, s) {
			h.Sources = append(h.Sources, s)
		}
	}
	slices.Sort(h.Sources)
	for _, a := range newer.Addrs {
		if !slices.Contains(h.Addrs, a) {
			h.Addrs = append(h.Addrs, a)
		}
	}
	if newer.Latency > 0 {
		h.Latency = newer.Latency
	}
}

// maxHosts caps the list, as LocalSSHDiscoveryManager does, so a hostile or
// enormous network cannot grow it without bound.
const maxHosts = 200

// Set accumulates hosts as they arrive from either source, deduplicating by
// name and by address. The zero Set is ready to use; it is not safe for
// concurrent use.
type Set struct {
	hosts []Host
}

// Add merges h in and reports whether the list changed.
func (s *Set) Add(h Host) bool {
	if h.Host == "" {
		return false
	}
	if h.Name == "" {
		h.Name = h.Host
	}
	if i := s.find(h); i >= 0 {
		before := s.hosts[i]
		merged := before
		merged.Addrs = slices.Clone(before.Addrs)
		merged.Sources = slices.Clone(before.Sources)
		// A Bonjour name is what the user should see and ssh to; a bare IP
		// only stands in until one turns up.
		if h.Bonjour() && !merged.Bonjour() {
			merged.Host = h.Host
		}
		merged.merge(h)
		s.hosts[i] = merged
		s.absorbAddresses(i)
		s.sort()
		return !equal(before, merged)
	}
	if len(s.hosts) >= maxHosts {
		return false
	}
	s.hosts = append(s.hosts, h)
	s.absorbAddresses(len(s.hosts) - 1)
	s.sort()
	return true
}

// List is the current hosts: Bonjour ones first, then by name, then by host.
func (s *Set) List() []Host {
	return slices.Clone(s.hosts)
}

// find returns the index of a host that is the same machine as h: the same
// ssh target, or an address either side already knows about.
func (s *Set) find(h Host) int {
	for i, have := range s.hosts {
		if strings.EqualFold(have.Host, h.Host) {
			return i
		}
		for _, a := range h.Addrs {
			if a == have.Host || slices.Contains(have.Addrs, a) {
				return i
			}
		}
		if slices.Contains(have.Addrs, h.Host) {
			return i
		}
	}
	return -1
}

// absorbAddresses folds into hosts[i] any bare-IP entry that turns out to be
// one of its addresses - a port-scan hit that arrived before Bonjour named it.
func (s *Set) absorbAddresses(i int) {
	keep := s.hosts[:0:0]
	target := s.hosts[i]
	for j, other := range s.hosts {
		if j != i && !other.Bonjour() && slices.Contains(target.Addrs, other.Host) {
			target.merge(other)
			continue
		}
		keep = append(keep, other)
	}
	for k := range keep {
		if strings.EqualFold(keep[k].Host, target.Host) {
			keep[k] = target
		}
	}
	s.hosts = keep
}

func (s *Set) sort() {
	slices.SortStableFunc(s.hosts, func(a, b Host) int {
		if a.Bonjour() != b.Bonjour() {
			if a.Bonjour() {
				return -1
			}
			return 1
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(strings.ToLower(a.Host), strings.ToLower(b.Host))
	})
}

func equal(a, b Host) bool {
	return a.Name == b.Name && a.Host == b.Host && a.Latency == b.Latency &&
		slices.Equal(a.Addrs, b.Addrs) && slices.Equal(a.Sources, b.Sources)
}
