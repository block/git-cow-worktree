package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// checkout brings the new worktree's working tree in line with its index.
// Hook execution is deliberately separate: checkout may be retried after a
// bad seeded index, but post-checkout must run at most once.
//
// Files we cloned and vouched for in the index are skipped here, so this
// writes only what's left; if seeding didn't happen, or the index we wrote
// was rejected, this degrades to an ordinary full checkout.
func checkout(repoDir string) error {
	cmd := exec.Command("git",
		"-c", "checkout.workers=0",
		"-c", "core.hooksPath=/dev/null",
		"checkout", "-f", "HEAD")
	cmd.Dir = repoDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Git owns and cleans up locks created by this child. Never infer lock
		// ownership from an earlier absence: another process may have created
		// the linked-worktree index.lock after such a check.
		return fmt.Errorf("git checkout: %w", err)
	}
	return nil
}

func runPostCheckoutHook(repoDir, targetSHA string) error {
	cmd := exec.Command("git", "hook", "run", "--ignore-missing", "post-checkout", "--", zeroObjectIDFor(targetSHA), targetSHA, "1")
	cmd.Dir = repoDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("post-checkout hook: %w", err)
	}
	return nil
}

func zeroObjectIDFor(oid string) string {
	return strings.Repeat("0", len(oid))
}

// resolveRev resolves a commit-ish to its full SHA within repoDir.
func resolveRev(repoDir, rev string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", rev+"^{commit}")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isCoWUnsupported reports whether err indicates the filesystem/pair
// doesn't support CoW reflinks (as opposed to a transient or per-file
// failure).
func isCoWUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errCoWUnsupported) {
		return true
	}
	// Underlying syscall errors:
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		err = pe.Err
	}
	for _, target := range cowUnsupportedErrnos {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
