// Package secret stores the peer's SSH password in the OS keychain.
//
// It is deliberately the only place in git-sync that holds a credential, and
// it holds it nowhere else: not in config.toml, not in debug.log, and never
// on a command line, where `ps` would show it to anyone on the machine.
package secret

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
)

// Service is the keychain service name every item is filed under.
const Service = "git-sync"

var errNotFound = errors.New("no password stored")

// IsNotFound reports whether err just means "nothing stored for that account".
// That is the ordinary key-auth case, not a failure.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

// Set stores password for account (a `user@host`), replacing any existing one.
func Set(account string, password []byte) error {
	switch backend() {
	case "file":
		return fileSet(account, password)
	case "blackhole":
		return nil // accepts writes, stores nothing - test-only, proves verify-by-reading-back
	case "security":
		// Interactive mode: the command goes in on stdin, so the password is
		// never an argv element. `-U` updates an existing item; `-T` names the
		// installed binary as an app allowed to read it without a prompt.
		cmd := exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(
			"add-generic-password -U -s %q -a %q -T %q -w %q\n",
			Service, account, config.BinPath(), password))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("storing the password in the keychain: %w: %s",
				err, redact(string(out), password))
		}
		return nil
	case "secret-tool":
		// secret-tool reads the password from stdin by design.
		cmd := exec.Command("secret-tool", "store", "--label="+Service,
			"service", Service, "account", account)
		cmd.Stdin = strings.NewReader(string(password))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("storing the password: %w: %s", err, out)
		}
		return nil
	}
	return errNoBackend()
}

func Get(account string) ([]byte, error) {
	switch backend() {
	case "file":
		return fileGet(account)
	case "blackhole":
		return nil, fmt.Errorf("%w for %s", errNotFound, account)
	case "security":
		out, err := exec.Command("security", "find-generic-password",
			"-s", Service, "-a", account, "-w").Output()
		if err != nil {
			return nil, fmt.Errorf("%w for %s", errNotFound, account)
		}
		return []byte(strings.TrimRight(string(out), "\n")), nil
	case "secret-tool":
		out, err := exec.Command("secret-tool", "lookup",
			"service", Service, "account", account).Output()
		if err != nil || len(out) == 0 {
			return nil, fmt.Errorf("%w for %s", errNotFound, account)
		}
		return out, nil
	}
	return nil, errNoBackend()
}

// Delete forgets the password. Deleting what is not there is a no-op, so
// uninstall does not have to check first.
func Delete(account string) error {
	switch backend() {
	case "file":
		return fileDelete(account)
	case "blackhole":
		return nil
	case "security":
		out, err := exec.Command("security", "delete-generic-password",
			"-s", Service, "-a", account).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "could not be found") {
			return fmt.Errorf("deleting the password: %w: %s", err, out)
		}
		return nil
	case "secret-tool":
		// secret-tool clear does not error on a missing item.
		return exec.Command("secret-tool", "clear",
			"service", Service, "account", account).Run()
	}
	return errNoBackend()
}

// Has is the cheap question the ssh path asks: is there a password for this
// account at all? Used to decide whether to arm the askpass helper.
func Has(account string) bool {
	_, err := Get(account)
	return err == nil
}

// backend picks the store. GITSYNC_SECRET_BACKEND=file is a test-only escape
// hatch: the real ones are shared OS state that a test must never write to,
// and on macOS reading one can raise a GUI prompt.
func backend() string {
	if b := os.Getenv("GITSYNC_SECRET_BACKEND"); b != "" {
		return b // "file" in tests; "blackhole" for the did-it-persist test
	}
	if runtime.GOOS == "darwin" {
		return "security"
	}
	if _, err := exec.LookPath("secret-tool"); err == nil {
		return "secret-tool"
	}
	return ""
}

func errNoBackend() error {
	return errors.New("no secret backend available: install libsecret (secret-tool) or run on macOS")
}

// redact replaces the password with *** anywhere it appears in text, before
// it can reach an error message or a log.
func redact(text string, password []byte) string {
	if len(password) == 0 {
		return text
	}
	return strings.ReplaceAll(text, string(password), "***")
}

// --- file backend: test-only, GITSYNC_SECRET_BACKEND=file ---
//
// One line per account: "account\tbase64(password)\n" in config.Home()/secrets,
// mode 0600. Base64 so an embedded tab or newline in a password can't corrupt
// the line format.

func secretsPath() string { return filepath.Join(config.Home(), "secrets") }

func fileSet(account string, password []byte) error {
	entries, err := readSecretsFile()
	if err != nil {
		return err
	}
	entries[account] = password
	return writeSecretsFile(entries)
}

func fileGet(account string) ([]byte, error) {
	entries, err := readSecretsFile()
	if err != nil {
		return nil, err
	}
	pw, ok := entries[account]
	if !ok {
		return nil, fmt.Errorf("%w for %s", errNotFound, account)
	}
	return pw, nil
}

func fileDelete(account string) error {
	entries, err := readSecretsFile()
	if err != nil {
		return err
	}
	if _, ok := entries[account]; !ok {
		return nil
	}
	delete(entries, account)
	return writeSecretsFile(entries)
}

func readSecretsFile() (map[string][]byte, error) {
	entries := make(map[string][]byte)
	b, err := os.ReadFile(secretsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return entries, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		pw, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		entries[parts[0]] = pw
	}
	return entries, nil
}

func writeSecretsFile(entries map[string][]byte) error {
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for account, pw := range entries {
		b.WriteString(account)
		b.WriteByte('\t')
		b.WriteString(base64.StdEncoding.EncodeToString(pw))
		b.WriteByte('\n')
	}
	tmp := secretsPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, secretsPath())
}
