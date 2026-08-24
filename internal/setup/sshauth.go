package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/grillermo/git-sync/internal/secret"
)

var errPasswordRequired = errors.New("peer wants a password")

// IsPasswordRequired reports whether ssh failed because the peer wants a
// password rather than because it could not be reached. ssh returns 255 for
// both, so the message is the only thing that tells them apart.
func IsPasswordRequired(err error) bool { return errors.Is(err, errPasswordRequired) }

// classify turns ssh's exit-255-for-everything into the distinction install
// actually has to make.
func classify(target string, out string, err error) error {
	if strings.Contains(out, "Permission denied") ||
		strings.Contains(out, "Authentication failed") ||
		strings.Contains(out, "Too many authentication failures") {
		return fmt.Errorf("%w: %s", errPasswordRequired, target)
	}
	return fmt.Errorf("%w: %s: %v", errPeerUnreachable, target, err)
}

// EnsureAuth makes sure this machine can reach the peer without a terminal
// later. If key auth already works it does nothing - the common case, and
// worth staying quiet about. Otherwise it asks for the password on the
// terminal, verifies it against the peer, and only then stores it.
//
// Verify before storing, always: an unverified password is worse than none,
// because every later sync fails in the background with a credential that was
// never right.
func EnsureAuth(target string, in io.Reader, out io.Writer) error {
	if _, err := Probe(target); err == nil {
		return nil // keys work
	} else if !IsPasswordRequired(err) {
		return err // unreachable: not something a password fixes
	}
	if in == nil {
		return fmt.Errorf(
			"%s asks for a password and there is no terminal to type it on; "+
				"set up an ssh key (ssh-copy-id %s) or run install interactively",
			target, target)
	}

	fmt.Fprintf(out, "%s wants a password. It is stored in your keychain and "+
		"used for every sync from now on.\n", target)

	// One scanner for the whole loop: a fresh bufio.Scanner per attempt would
	// buffer ahead into `in` and silently swallow the next attempt's line
	// whenever in is not itself line-buffered (a strings.Reader, a pipe).
	sc := bufio.NewScanner(in)

	const attempts = 3
	for i := 0; i < attempts; i++ {
		pw, err := readPassword(in, sc, out, "password for "+target+": ")
		if err != nil {
			return err
		}
		// Store it, try it, and unstore it if it did not work: the askpass
		// helper reads from the keychain, so there is no other way to test a
		// password than to put it there first.
		if err := secret.Set(target, pw); err != nil {
			zero(pw)
			return err
		}
		zero(pw)

		// Read it back before even trying the peer. A keychain write can
		// report success and still not persist - a locked keychain, a
		// denied prompt, a secret-tool with no running daemon - and there is
		// no point ssh-ing anywhere with a password the store already lost;
		// the cost of not catching this here is believing it lands on the
		// next commit, in the background, where nobody is watching.
		if _, err := secret.Get(target); err != nil {
			return fmt.Errorf(
				"the password could not be saved to the keychain: %w\n"+
					"git-sync cannot ask again from a commit hook, so it will not "+
					"sync until this is fixed - unlock the keychain, or set up an "+
					"ssh key with ssh-copy-id %s", err, target)
		}

		if _, err := Probe(target); err == nil {
			fmt.Fprintf(out, "password accepted and saved to the keychain for %s\n", target)
			return nil
		}
		_ = secret.Delete(target)
		fmt.Fprintln(out, "that password was not accepted")
	}
	return fmt.Errorf("%s did not accept the password after %d attempts", target, attempts)
}

// readPassword prints prompt to out and reads a password. When in is
// literally os.Stdin and it is a terminal, it uses term.ReadPassword so real
// interactive use does not echo the password; otherwise (all the tests, which
// pass a strings.Reader) it reads the next line from sc, a scanner over in
// that the caller creates once and reuses across retries - a fresh Scanner
// per attempt would buffer ahead into in and lose whatever it did not return.
func readPassword(in io.Reader, sc *bufio.Scanner, out io.Writer, prompt string) ([]byte, error) {
	fmt.Fprint(out, prompt)
	if f, ok := in.(*os.File); ok && f == os.Stdin && term.IsTerminal(int(f.Fd())) {
		pw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return nil, fmt.Errorf("reading the password: %w", err)
		}
		return pw, nil
	}
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("reading the password: %w", err)
		}
		return nil, errors.New("no password given")
	}
	return []byte(sc.Text()), nil
}

// zero overwrites a password's bytes once the caller is done with it.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
