package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func validateWorktree(worktree, targetSHA string) error {
	got, err := resolveRev(worktree, "HEAD")
	if err != nil {
		return err
	}
	if got != targetSHA {
		return fmt.Errorf("HEAD mismatch")
	}
	cmd := exec.Command("git", "-c", "core.fsmonitor=false", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = worktree
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git status: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if len(out) != 0 {
		return fmt.Errorf("worktree is not clean")
	}
	return nil
}

func moveWorktree(repoDir, oldPath, newPath string) error {
	// `git worktree move old existing-empty-dir` nests old under that dir,
	// unlike `worktree add`, which accepts the empty directory as the target.
	// Remove only a pre-existing empty target, then publish to the exact path.
	if entries, err := os.ReadDir(newPath); err == nil {
		if len(entries) != 0 {
			return fmt.Errorf("target directory is not empty")
		}
		if err := os.Remove(newPath); err != nil {
			return fmt.Errorf("remove empty target: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect target: %w", err)
	}
	// Two force flags are required when the user requested --lock; moving the
	// staging worktree is still safe because both paths belong to this run.
	cmd := exec.Command("git", "worktree", "move", "--force", "--force", oldPath, newPath)
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git worktree move: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
