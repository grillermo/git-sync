package picker

import (
	"reflect"
	"testing"
	"time"

	"github.com/grillermo/chicle"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/discovery"
)

func studioHost() discovery.Host {
	return discovery.Host{
		Name: "Studio", Host: "192.168.1.5", Hostname: "studio.local", Addrs: []string{"192.168.1.5"},
		Sources: []discovery.Source{discovery.SourceBonjour, discovery.SourcePortScan},
		Latency: 4 * time.Millisecond,
	}
}

func TestPeerRowsOnAFirstInstallHaveNoHeadings(t *testing.T) {
	rows := PeerRows([]discovery.Host{studioHost()}, nil)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Key != "192.168.1.5" || r.Section != "" || r.Ticked || r.Locked {
		t.Errorf("row = %+v", r)
	}
	if want := []string{"Studio", "192.168.1.5", "bonjour, port 22   4ms"}; !reflect.DeepEqual(r.Cols, want) {
		t.Errorf("cols = %q, want %q", r.Cols, want)
	}
}

func TestPeerRowsShowConfiguredPeersLockedAndOnlyOnce(t *testing.T) {
	current := []config.Peer{{Host: "192.168.1.5", User: "g"}, {Host: "laptop.local", User: "g"}}
	rows := PeerRows([]discovery.Host{studioHost()}, current)

	// Studio owns 192.168.1.5, so it is the configured peer, not a new find.
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want just the two configured peers", rows)
	}
	for _, r := range rows {
		if r.Section != sectionPeers || !r.Ticked || !r.Locked {
			t.Errorf("configured peer row = %+v, want ticked, locked, under %q", r, sectionPeers)
		}
	}
}

func TestPeerRowsRecogniseAPeerConfiguredByItsOldName(t *testing.T) {
	rows := PeerRows([]discovery.Host{studioHost()}, []config.Peer{{Host: "Studio.local", User: "g"}})
	if len(rows) != 1 || rows[0].Section != sectionPeers {
		t.Errorf("rows = %+v, want only the configured peer", rows)
	}
}

func TestPeerRowsPutNewFindsFirstUnderAHeading(t *testing.T) {
	rows := PeerRows([]discovery.Host{studioHost()}, []config.Peer{{Host: "laptop.local", User: "g"}})
	if len(rows) != 2 || rows[0].Key != "192.168.1.5" || rows[0].Section != sectionFound {
		t.Errorf("rows = %+v", rows)
	}
}

func TestChoosePeersStreamsFindsIntoThePicker(t *testing.T) {
	found := make(chan discovery.Host)
	go func() {
		found <- studioHost()
		close(found)
	}()

	var sawStudio bool
	hosts, ok, err := choosePeers(func(cfg chicle.Config) (string, error) {
		if !cfg.MultiSelect || cfg.Updates == nil {
			t.Fatal("the machine picker is multi-select and fills in live")
		}
		for rows := range cfg.Updates {
			for _, r := range rows {
				sawStudio = sawStudio || r.Key == "192.168.1.5"
			}
		}
		return encode([]string{"192.168.1.5", "laptop.local"}), nil
	}, found, []config.Peer{{Host: "laptop.local", User: "g"}})

	if err != nil || !ok {
		t.Fatalf("choosePeers = %v, %v", ok, err)
	}
	if !sawStudio {
		t.Error("a host the scan found never reached the picker")
	}
	if !reflect.DeepEqual(hosts, []string{"192.168.1.5"}) {
		t.Errorf("hosts = %v, want only the newly ticked 192.168.1.5", hosts)
	}
}

func TestChoosePeersDoesNotHangWhenTheUserFinishesMidScan(t *testing.T) {
	// Not closed until the test ends: the scan is still going when the user
	// saves, so the pump has a find in hand and nobody reading its updates.
	found := make(chan discovery.Host, 1)
	defer close(found)
	found <- studioHost()

	finished := make(chan struct{})
	go func() {
		_, ok, _ := choosePeers(func(chicle.Config) (string, error) { return "", nil }, found, nil)
		if ok {
			t.Error("a quit must read as cancelled")
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("choosePeers blocked on a scan the picker no longer listens to")
	}
}
