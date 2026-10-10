package syncer

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/sshx"
)

// Announce is what the login service runs when this machine starts: it tells
// the mesh this machine is back, so every peer sends what it could not
// deliver while this one was off, then catches this machine up and sends
// its own backlog.
//
// A login service usually starts before the network is up, so every step is
// retried with backoff until it gets through or announceTimeout runs out. A
// peer that stays unreachable is not an error: it is presumably off too, and
// will announce itself when it starts.
//
// Returns a process exit code.
func Announce() int {
	return announce(nil)
}

// announce is Announce, abandoned early once stop is closed. It checks stop
// only between repos and between rounds, never inside one, so a stop can
// never land between a receive's stash and its unstash.
func announce(stop <-chan struct{}) int {
	cfg, err := config.Load()
	if err != nil {
		return 1
	}

	// Catch up from the remote directly, not only through the peers: the
	// machine that made a commit may itself be off by now, and then nobody
	// would ever tell us about it.
	repos := map[string]bool{}
	for _, rel := range cfg.Repos {
		if retryable(cfg, rel) {
			repos[rel] = true
		}
	}
	peers := map[string]config.Peer{}
	for _, p := range cfg.PeerList() {
		peers[p.Host] = p
	}

	deadline := time.Now().Add(announceTimeout())
	wait := time.Second
	for {
		for rel := range repos {
			if stopped(stop) {
				return 0
			}
			if catchUp(rel) {
				delete(repos, rel)
			}
		}
		for host := range tellPeers(peers) {
			delete(peers, host)
		}
		// Our own backlog: notifications this machine could not send before
		// it went down. Cheap when there is none.
		retryPending(cfg, "", nil)

		if len(repos) == 0 && len(peers) == 0 {
			break
		}
		if time.Now().Add(wait).After(deadline) {
			for host := range peers {
				activity.AppendDebug("announce: " + host + " unreachable; it catches up when it starts")
			}
			for rel := range repos {
				_ = activity.Append(activity.Event{
					Repo: rel, Op: activity.OpReceive, Status: activity.StatusOffline,
					Msg: "could not reach the remote at startup; catches up on the next sync",
				})
			}
			break
		}
		select {
		case <-stop:
			return 0
		case <-time.After(wait):
		}
		if wait *= 2; wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
	return 0
}

func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// catchUp brings one repo up to date from the remote, exactly as a receive
// would. It reports false only when the remote could not be reached, the one
// outcome waiting can fix.
func catchUp(rel string) bool {
	return receive(rel, "the remote (startup catch-up)") != ExitFetchFailed
}

// tellPeers asks every peer, in parallel, to send this machine its backlog,
// and returns the hosts that answered. A peer's retry also flushes what it
// owes the rest of the mesh, which does no harm.
func tellPeers(peers map[string]config.Peer) map[string]bool {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		told = map[string]bool{}
	)
	for host, p := range peers {
		wg.Add(1)
		go func(host string, p config.Peer) {
			defer wg.Done()
			out, err := sshx.Command(p.Target(), "~/.gitsync/bin/git-sync retry").CombinedOutput()
			switch code := exitCode(err); {
			case code == 255:
				return // down, or our network is not up yet: try again
			case err != nil:
				// Reached it but it could not retry - most likely an older
				// binary without the subcommand. Waiting will not fix that.
				activity.AppendDebug(fmt.Sprintf("announce: %s retry failed (exit %d): %s",
					host, code, strings.TrimSpace(string(out))))
			default:
				activity.AppendDebug("announce: told " + host + " this machine is back")
			}
			mu.Lock()
			told[host] = true
			mu.Unlock()
		}(host, p)
	}
	wg.Wait()
	return told
}

// announceTimeout is how long Announce keeps trying. Overridable so tests do
// not wait out the real five minutes.
func announceTimeout() time.Duration {
	if s := os.Getenv("GITSYNC_ANNOUNCE_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return 5 * time.Minute
}
