package discovery

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// bonjourTypes are the services an sshd advertises: macOS's Remote Login
// publishes both, and avahi's stock ssh.service publishes the first.
var bonjourTypes = []string{"_ssh._tcp.local.", "_sftp-ssh._tcp.local."}

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// resendAfter are the offsets at which the PTR query goes out again. mDNS is
// best-effort multicast UDP; a responder that was asleep, or a packet that
// was dropped, would otherwise never be heard from in this scan.
var resendAfter = []time.Duration{time.Second, 3 * time.Second}

// browse queries for sshd adverts from an ephemeral port - a "legacy unicast"
// query in RFC 6762 terms, which responders answer straight back to that
// port. That sidesteps binding 5353, which mDNSResponder or avahi already
// holds. It runs for d, then reports any service that never resolved under
// its instance name, as vvterm's didNotResolve fallback does.
func browse(ctx context.Context, d time.Duration, found func(Host)) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return err
	}
	defer conn.Close()

	cache := newMDNSCache()
	send := func(qs []dnsmessage.Question) {
		if pkt, err := query(qs); err == nil {
			_, _ = conn.WriteToUDP(pkt, mdnsGroup)
		}
	}
	send(ptrQuestions())

	started := time.Now()
	deadline := started.Add(d)
	resends := resendAfter
	buf := make([]byte, 9000)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		if len(resends) > 0 && time.Since(started) >= resends[0] {
			resends = resends[1:]
			send(ptrQuestions())
		}
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if cache.absorb(buf[:n]) != nil {
			continue
		}
		for _, h := range cache.take() {
			found(h)
		}
		if qs := cache.followUps(); len(qs) > 0 {
			send(qs)
		}
	}
	for _, h := range cache.fallbacks() {
		found(h)
	}
	return nil
}

func ptrQuestions() []dnsmessage.Question {
	var qs []dnsmessage.Question
	for _, t := range bonjourTypes {
		qs = append(qs, dnsmessage.Question{
			Name: dnsmessage.MustNewName(t), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET,
		})
	}
	return qs
}

func query(qs []dnsmessage.Question) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	for _, q := range qs {
		if err := b.Question(q); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

type srvRecord struct {
	target string
	port   uint16
}

// mdnsCache stitches answers together across packets: a responder may send
// the PTR, the SRV that resolves it and the A records for the SRV's target in
// one packet or in three.
type mdnsCache struct {
	instances map[string]string // instance FQDN, lowercased -> as advertised
	names     map[string]string
	srv       map[string]srvRecord
	addrs     map[string][]string // target FQDN (lowercased) -> IPv4s
	reported  map[string]int      // instance -> addresses known when last reported
	asked     map[string]bool     // instance -> SRV follow-up already sent
}

func newMDNSCache() *mdnsCache {
	return &mdnsCache{
		instances: map[string]string{}, names: map[string]string{},
		srv: map[string]srvRecord{}, addrs: map[string][]string{},
		reported: map[string]int{}, asked: map[string]bool{},
	}
}

// absorb records every PTR, SRV and A record in one mDNS packet, whichever
// section it arrived in.
func (c *mdnsCache) absorb(msg []byte) error {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return err
	}
	if !h.Response {
		return errors.New("not a response")
	}
	if err := p.SkipAllQuestions(); err != nil {
		return err
	}
	for {
		rh, err := nextHeader(&p)
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return nil
		}
		if err != nil {
			return err
		}
		owner := strings.ToLower(rh.Name.String())
		switch rh.Type {
		case dnsmessage.TypePTR:
			r, err := p.PTRResource()
			if err != nil {
				return err
			}
			if !isBonjourType(owner) {
				continue
			}
			inst := r.PTR.String()
			key := strings.ToLower(inst)
			c.instances[key] = inst
			c.names[key] = instanceName(inst, owner)
		case dnsmessage.TypeSRV:
			r, err := p.SRVResource()
			if err != nil {
				return err
			}
			c.srv[owner] = srvRecord{target: r.Target.String(), port: r.Port}
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return err
			}
			ip := net.IP(r.A[:]).String()
			if !contains(c.addrs[owner], ip) {
				c.addrs[owner] = append(c.addrs[owner], ip)
			}
		default:
			if err := skip(&p); err != nil {
				return err
			}
		}
	}
}

// nextHeader walks answers, then authorities, then additionals, as one
// stream: mDNS responders put the records that matter in any of them.
func nextHeader(p *dnsmessage.Parser) (dnsmessage.ResourceHeader, error) {
	for _, next := range []func() (dnsmessage.ResourceHeader, error){
		p.AnswerHeader, p.AuthorityHeader, p.AdditionalHeader,
	} {
		h, err := next()
		if !errors.Is(err, dnsmessage.ErrSectionDone) {
			return h, err
		}
	}
	return dnsmessage.ResourceHeader{}, dnsmessage.ErrSectionDone
}

// skip discards the body of whichever section's resource was last read; the
// parser only accepts the skip for the section it is in.
func skip(p *dnsmessage.Parser) error {
	for _, s := range []func() error{p.SkipAnswer, p.SkipAuthority, p.SkipAdditional} {
		if err := s(); err == nil {
			return nil
		}
	}
	return errors.New("could not skip resource")
}

// take returns every instance that now resolves to a host on port 22 with at
// least one IPv4, and was not already reported with at least this many
// addresses. Only port 22: a peer is config.Peer{User, Host}, with nowhere to
// put a port. No address yet means nothing to ssh to - the host is reported
// once an A record arrives, never by its .local name.
func (c *mdnsCache) take() []Host {
	var out []Host
	for inst := range c.instances {
		s, ok := c.srv[inst]
		if !ok || s.port != sshPort {
			continue
		}
		target := strings.ToLower(s.target)
		addrs := c.addrs[target]
		if len(addrs) == 0 {
			continue
		}
		if n, done := c.reported[inst]; done && n >= len(addrs) {
			continue
		}
		c.reported[inst] = len(addrs)
		out = append(out, Host{
			Name: c.names[inst], Host: bestAddr(addrs), Hostname: strings.TrimSuffix(s.target, "."),
			Addrs: append([]string(nil), addrs...), Sources: []Source{SourceBonjour},
		})
	}
	return out
}

// bestAddr picks the address to ssh to from a Bonjour host's A records: the
// first that is not link-local (169.254/16, a self-assigned address on a
// cable or bridge rather than the LAN), else the first.
func bestAddr(addrs []string) string {
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && !ip.IsLinkLocalUnicast() {
			return a
		}
	}
	return addrs[0]
}

// followUps asks for the SRV (and the A records the responder sends with it)
// of every instance a PTR named but no SRV has yet resolved - once each.
func (c *mdnsCache) followUps() []dnsmessage.Question {
	var qs []dnsmessage.Question
	for inst, advertised := range c.instances {
		if _, ok := c.srv[inst]; ok || c.asked[inst] {
			continue
		}
		c.asked[inst] = true
		name, err := dnsmessage.NewName(advertised)
		if err != nil {
			continue
		}
		qs = append(qs, dnsmessage.Question{Name: name, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET})
	}
	return qs
}

// fallbacks names each instance that never resolved as <instance>.local, the
// hostname a Mac derives from its computer name - usually right, and install's
// reachability check will say so if it is not.
func (c *mdnsCache) fallbacks() []Host {
	var out []Host
	for inst := range c.instances {
		if _, ok := c.srv[inst]; ok {
			continue
		}
		host := sanitizedLocalHostName(c.names[inst]) + ".local"
		out = append(out, Host{Name: c.names[inst], Host: host, Sources: []Source{SourceBonjour}})
	}
	return out
}

func isBonjourType(owner string) bool {
	for _, t := range bonjourTypes {
		if owner == t {
			return true
		}
	}
	return false
}

// instanceName is the human part of an instance FQDN: "Studio._ssh._tcp.local."
// is "Studio".
func instanceName(inst, serviceType string) string {
	lower := strings.ToLower(inst)
	if strings.HasSuffix(lower, "."+serviceType) {
		return inst[:len(inst)-len(serviceType)-1]
	}
	return strings.TrimSuffix(inst, ".")
}

var (
	whitespace  = regexp.MustCompile(`\s+`)
	notHostChar = regexp.MustCompile(`[^a-z0-9-]`)
)

// sanitizedLocalHostName turns "Guillermo’s Mac mini" into
// "guillermos-mac-mini", the way macOS derives its LocalHostName.
func sanitizedLocalHostName(name string) string {
	s := whitespace.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = notHostChar.ReplaceAllString(s, "")
	if s == "" {
		return name
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
