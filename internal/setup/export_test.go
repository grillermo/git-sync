package setup

// ServiceFor exposes the per-OS unit to the external tests, so both
// platforms' units are checked whichever one runs the suite.
func ServiceFor(goos, home, bin, logPath string) (path, content, link string, err error) {
	s, err := serviceFor(goos, home, bin, logPath)
	return s.path, s.content, s.link, err
}
