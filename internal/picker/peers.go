package picker

import (
	"fmt"
	"slices"
	"strings"

	"github.com/grillermo/chicle"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/discovery"
)

const (
	sectionFound = "FOUND ON THIS NETWORK"
	sectionPeers = "ALREADY IN THE MESH"
)

// PeerRows builds the machine list: what the scan found so far, then the
// peers already configured. A found host that is already a peer is shown
// once, as the peer.
func PeerRows(found []discovery.Host, current []config.Peer) []chicle.Row {
	var news, peers []chicle.Row
	for _, h := range found {
		if isPeer(h, current) {
			continue
		}
		news = append(news, chicle.Row{
			Key:     h.Host,
			Cols:    []string{h.Name, h.Host, describeHost(h)},
			Section: sectionFound,
		})
	}
	for _, p := range current {
		// Already in the mesh: locked, as in the repo picker - removing a
		// machine is `git-sync uninstall` on it, not an untick here.
		peers = append(peers, chicle.Row{
			Key:     p.Host,
			Cols:    []string{p.Host, p.Target(), "configured"},
			Section: sectionPeers,
			Ticked:  true,
			Locked:  true,
		})
	}
	if len(peers) == 0 {
		for i := range news {
			news[i].Section = ""
		}
	}
	return append(news, peers...)
}

// PeerConfig is the machine picker. rows is the first draw; updates carries
// every later one as the scan finds more.
func PeerConfig(rows []chicle.Row, updates <-chan []chicle.Row) chicle.Config {
	return chicle.Config{
		Title:       "SELECT MACHINES TO SYNC WITH (scanning Bonjour and port 22 on this network)",
		Columns:     []chicle.Column{{Title: "MACHINE", Width: 28}, {Title: "SSH TO", Width: 32}, {Title: "FOUND BY"}},
		Rows:        rows,
		MultiSelect: true,
		Updates:     updates,
		Actions: []chicle.Action{
			{Label: "Save", Run: func(s chicle.Selection) chicle.Outcome {
				hosts := make([]string, len(s.Ticked))
				for i, r := range s.Ticked {
					hosts[i] = r.Key
				}
				return chicle.Outcome{Result: encode(hosts), Done: true}
			}},
			{Label: "Cancel"},
		},
	}
}

// ChoosePeers shows the machine picker while found is still filling in, and
// returns the hosts ticked that are not already in current. ok is false when
// the user cancelled. The caller owns the scan behind found and must stop it
// once this returns.
func ChoosePeers(found <-chan discovery.Host, current []config.Peer) (hosts []string, ok bool, err error) {
	return choosePeers(chicle.Run, found, current)
}

func choosePeers(run func(chicle.Config) (string, error), found <-chan discovery.Host,
	current []config.Peer) ([]string, bool, error) {
	updates := make(chan []chicle.Row)
	done := make(chan struct{})
	go func() {
		defer close(updates)
		var set discovery.Set
		for h := range found {
			if !set.Add(h) {
				continue
			}
			select {
			case updates <- PeerRows(set.List(), current):
			case <-done:
				return
			}
		}
	}()

	res, err := run(PeerConfig(PeerRows(nil, current), updates))
	close(done)
	if err != nil {
		return nil, false, err
	}
	ticked, ok := decode(res)
	if !ok {
		return nil, false, nil
	}
	var out []string
	for _, h := range ticked {
		if !slices.ContainsFunc(current, func(p config.Peer) bool { return strings.EqualFold(p.Host, h) }) {
			out = append(out, h)
		}
	}
	return out, true, nil
}

// isPeer reports whether h is a machine already configured, by name or by
// one of its addresses.
func isPeer(h discovery.Host, current []config.Peer) bool {
	for _, p := range current {
		if strings.EqualFold(p.Host, h.Host) || slices.Contains(h.Addrs, p.Host) {
			return true
		}
	}
	return false
}

// describeHost is the FOUND BY column: which sources saw it, and how fast
// port 22 answered when the sweep reached it.
func describeHost(h discovery.Host) string {
	parts := make([]string, len(h.Sources))
	for i, s := range h.Sources {
		parts[i] = string(s)
	}
	s := strings.Join(parts, ", ")
	if h.Latency > 0 {
		s += fmt.Sprintf("   %dms", h.Latency.Milliseconds())
	}
	return s
}
