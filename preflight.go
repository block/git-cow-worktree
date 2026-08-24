package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// cowPreflight proves that this source/target filesystem pair can reflink
// before auto mode registers a worktree. The target's parent is used because
// the target itself must not exist yet for `git worktree add`.
func cowPreflight(sourceRoot, target string) error {
	if os.Getenv("GIT_COW_WORKTREE_FORCE_NO_COW") != "" {
		return errCoWUnsupported
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return fmt.Errorf("create target parent: %w", err)
	}

	src, err := os.CreateTemp(sourceRoot, ".git-cow-worktree-preflight-*")
	if err != nil {
		return fmt.Errorf("create source probe: %w", err)
	}
	srcName := src.Name()
	defer os.Remove(srcName)
	if _, err := src.WriteString("git-cow-worktree preflight\n"); err != nil {
		src.Close()
		return fmt.Errorf("write source probe: %w", err)
	}
	if err := src.Close(); err != nil {
		return fmt.Errorf("close source probe: %w", err)
	}

	dst, err := os.CreateTemp(parent, ".git-cow-worktree-preflight-dst-*")
	if err != nil {
		return fmt.Errorf("create target probe: %w", err)
	}
	dstName := dst.Name()
	dst.Close()
	if err := os.Remove(dstName); err != nil {
		return fmt.Errorf("prepare target probe: %w", err)
	}
	defer os.Remove(dstName)
	if err := cowClone(srcName, dstName); err != nil {
		if isCoWUnsupported(err) {
			return fmt.Errorf("CoW capability probe: %w", errCoWUnsupported)
		}
		return fmt.Errorf("CoW capability probe: %w", err)
	}
	got, err := os.ReadFile(dstName)
	if err != nil {
		return fmt.Errorf("read target probe: %w", err)
	}
	if string(got) != "git-cow-worktree preflight\n" {
		return errors.New("CoW capability probe copied incorrect content")
	}
	return nil
}
