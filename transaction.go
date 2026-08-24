package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type worktreeTransaction struct {
	repoDir    string
	targetAbs  string
	refsBefore map[string]string
	changedRef string
	oldOID     string
	newOID     string
	registered bool
	committed  bool
}

func localBranchRefs(repoDir string) (map[string]string, error) {
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname)%00%(objectname)", "refs/heads/")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		ref, oid, ok := bytes.Cut(line, []byte{0})
		if !ok {
			continue
		}
		refs[string(ref)] = string(oid)
	}
	return refs, nil
}

func (tx *worktreeTransaction) captureCreatedRef(headOID string) {
	cmd := exec.Command("git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = tx.targetAbs
	out, err := cmd.Output()
	if err != nil {
		return // detached HEAD
	}
	ref := strings.TrimSpace(string(out))
	oldOID, existed := tx.refsBefore[ref]
	if !existed || oldOID != headOID {
		tx.changedRef, tx.oldOID, tx.newOID = ref, oldOID, headOID
	}
}

func (tx *worktreeTransaction) rollback() error {
	if tx.committed {
		return nil
	}
	var errs []error
	// Two force flags are required for a transaction-created locked worktree.
	// Never use repository-wide prune here: rollback may touch only resources
	// this operation owns.
	cmd := exec.Command("git", "worktree", "remove", "--force", "--force", tx.targetAbs)
	cmd.Dir = tx.repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		errs = append(errs, fmt.Errorf("git worktree remove: %w (%s)", err, strings.TrimSpace(string(out))))
		return errors.Join(errs...)
	}
	if tx.changedRef != "" {
		// Compare-and-swap the branch back to its pre-operation value. If
		// another process moved it, preserve that update rather than rolling it
		// back as if it were ours.
		var cmd *exec.Cmd
		if tx.oldOID == "" {
			cmd = exec.Command("git", "update-ref", "-d", tx.changedRef, tx.newOID)
		} else {
			cmd = exec.Command("git", "update-ref", tx.changedRef, tx.oldOID, tx.newOID)
		}
		cmd.Dir = tx.repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("restore branch %s: %w (%s)", tx.changedRef, err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}
