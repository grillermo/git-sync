// Package sshx builds the ssh commands git-sync runs, with the peer's stored
// password wired in when there is one.
package sshx

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/secret"
)

// askpassShimTemplate mirrors internal/setup/install.go's askpassShimTemplate:
// ssh execs SSH_ASKPASS with no way to pass it extra arguments, so the
// account has to be baked into the file. `git-sync install` writes the
// persistent copy of this once config.BinPath() holds the installed binary;
// this package writes (or refreshes) the same shim on demand, using whatever
// binary is currently running, so a password can be verified during install
// itself - before that persistent copy exists.
const askpassShimTemplate = "#!/bin/sh\n" +
	"# Written by git-sync so ssh can read a stored password from the keychain.\n" +
	"exec %q askpass %q \"$@\"\n"

// Command returns an ssh command for `remote`, run as account (user@host).
//
// With no stored password, BatchMode=yes: no prompt can ever be answered from
// a detached hook, so failing immediately is the only honest option.
//
// With one, BatchMode has to be off - it disables askpass too - and the
// password comes from the askpass shim instead. NumberOfPasswordPrompts=1
// keeps a wrong stored password from turning into three silent retries.
func Command(account, remote string) *exec.Cmd {
	args := []string{"-o", "ConnectTimeout=5"}
	if secret.Has(account) {
		args = append(args, "-o", "BatchMode=no", "-o", "NumberOfPasswordPrompts=1")
	} else {
		args = append(args, "-o", "BatchMode=yes")
	}
	args = append(args, account, remote)

	cmd := exec.Command("ssh", args...)
	cmd.Env = Env(account)
	return cmd
}

// Env is the process environment ssh needs to answer a password prompt
// without a terminal. SSH_ASKPASS_REQUIRE=force is what makes ssh use the
// helper even when a tty is attached (OpenSSH 8.4+); DISPLAY is set because
// older ssh refuses to use an askpass helper without one.
func Env(account string) []string {
	env := os.Environ()
	if !secret.Has(account) {
		return env
	}
	// Best-effort: if this fails, ssh's own prompt failure (no terminal, no
	// working askpass) reports the problem loudly enough on its own.
	_ = ensureAskpassShim(account)
	return append(env,
		"SSH_ASKPASS="+config.AskpassPath(),
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=:0",
	)
}

// ensureAskpassShim writes the askpass helper for account, using whichever
// binary is currently running. In a real install this is the git-sync
// binary itself - the same one `git-sync install` will (or already did) copy
// to config.BinPath() - so the shim works whether or not install has
// finished writing its own persistent copy yet.
func ensureAskpassShim(account string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	content := fmt.Sprintf(askpassShimTemplate, self, account)
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	tmp := config.AskpassPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o700); err != nil {
		return err
	}
	return os.Rename(tmp, config.AskpassPath())
}
