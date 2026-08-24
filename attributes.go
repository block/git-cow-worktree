package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

var conversionAttributes = map[string]bool{
	"filter": true, "working-tree-encoding": true, "ident": true,
	"text": true, "eol": true, "crlf": true,
}

// conversionExclusions returns paths whose target working-tree representation
// can differ from the blob stored in Git. Such paths must be materialized by
// Git so clean/smudge, LFS, encoding, ident, and EOL semantics are preserved.
func conversionExclusions(repoDir, targetSHA string, tree *treeIndex) (map[string]bool, error) {
	if conversionConfigEnabled(repoDir) {
		excluded := make(map[string]bool, len(tree.Blobs))
		for path := range tree.Blobs {
			excluded[path] = true
		}
		return excluded, nil
	}
	if len(tree.Blobs) == 0 {
		return nil, nil
	}
	// --all emits only attributes that have rules, avoiding six output rows
	// for every path in large repositories where almost all are unspecified.
	args := []string{"check-attr", "-z", "--source=" + targetSHA, "--all", "--stdin"}
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	var input bytes.Buffer
	for path, entry := range tree.Blobs {
		if entry.IsRegular() {
			input.WriteString(path)
			input.WriteByte(0)
		}
	}
	cmd.Stdin = &input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git check-attr: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	fields := bytes.Split(out, []byte{0})
	excluded := make(map[string]bool)
	for i := 0; i+2 < len(fields); i += 3 {
		if conversionAttributes[string(fields[i+1])] && string(fields[i+2]) != "unspecified" {
			excluded[string(fields[i])] = true
		}
	}
	return excluded, nil
}

func conversionConfigEnabled(repoDir string) bool {
	for _, key := range []string{"core.autocrlf", "core.eol"} {
		cmd := exec.Command("git", "config", "--get", key)
		cmd.Dir = repoDir
		out, err := cmd.Output()
		if err == nil {
			value := strings.TrimSpace(string(out))
			if key == "core.autocrlf" && value != "false" || key == "core.eol" && value != "native" {
				return true
			}
		}
	}
	return false
}
