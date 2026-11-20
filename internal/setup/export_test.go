package setup

// ServiceFor exposes the per-OS unit to the external tests, so both
// platforms' units are checked whichever one runs the suite.
func ServiceFor(goos, home, bin, logPath string) (path, content, link string, err error) {
	s, err := serviceFor(goos, home, bin, logPath)
	return s.path, s.content, s.link, err
}

// RepoCheckScript and SyncMeasureScript expose the remote shell scripts so a
// test can run them under each login shell a peer might have.
func RepoCheckScript(peerBase string, repos, remotePrefs []string) string {
	return repoCheckScript(peerBase, repos, remotePrefs)
}

func SyncMeasureScript(peerBase string, repos, remotePrefs []string) string {
	return syncMeasureScript(peerBase, repos, remotePrefs)
}
