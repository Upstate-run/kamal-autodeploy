// Package gitx runs the few read-only git commands selection needs. It never runs hooks,
// never checks anything out and passes revisions only after validating them as full SHAs.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidSHA is a full lowercase 40-character commit SHA.
func ValidSHA(s string) bool { return shaPattern.MatchString(s) }

// Repo is what selection needs from the checkout; tests substitute a fake.
type Repo interface {
	HasCommit(sha string) bool
	// IsAncestor reports whether a is an ancestor of (or equal to) b.
	IsAncestor(a, b string) (bool, error)
	// ChangedFiles lists every path that differs between two trees. Renames appear as both
	// the old and the new path, so neither side's owner is missed.
	ChangedFiles(from, to string) ([]string, error)
	// FirstParents lists up to n commits on ref's first-parent chain, newest first.
	FirstParents(ref string, n int) ([]string, error)
	CommitMessage(sha string) (string, error)
	// Head is the checked-out commit.
	Head() (string, error)
}

// Git runs git in Dir.
type Git struct{ Dir string }

func (g Git) run(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = g.Dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("git %s failed", args[0])
	}
	return out.String(), nil
}

func (g Git) HasCommit(sha string) bool {
	if !ValidSHA(sha) {
		return false
	}
	_, err := g.run("cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func (g Git) IsAncestor(a, b string) (bool, error) {
	if !ValidSHA(a) || !ValidSHA(b) {
		return false, errors.New("invalid commit")
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = g.Dir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, errors.New("git merge-base failed")
}

func (g Git) ChangedFiles(from, to string) ([]string, error) {
	if !ValidSHA(from) || !ValidSHA(to) {
		return nil, errors.New("invalid commit")
	}
	out, err := g.run("diff", "--no-renames", "--name-only", "-z", from, to, "--")
	if err != nil {
		return nil, err
	}
	files := []string{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

func (g Git) FirstParents(ref string, n int) ([]string, error) {
	if !ValidSHA(ref) && !strings.HasPrefix(ref, "refs/remotes/origin/") {
		return nil, errors.New("invalid ref")
	}
	out, err := g.run("rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", n), ref, "--")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (g Git) Head() (string, error) {
	out, err := g.run("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !ValidSHA(sha) {
		return "", errors.New("HEAD is not a commit")
	}
	return sha, nil
}

// Tracked reports whether path is tracked in the index.
func (g Git) Tracked(path string) bool {
	_, err := g.run("ls-files", "--error-unmatch", "--", path)
	return err == nil
}

// Ignored reports whether path is ignored by .gitignore rules.
func (g Git) Ignored(path string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", "--no-index", "--", path)
	cmd.Dir = g.Dir
	return cmd.Run() == nil
}

// CommitMessage returns immutable commit metadata without invoking hooks.
func (g Git) CommitMessage(sha string) (string, error) {
	if !ValidSHA(sha) {
		return "", errors.New("invalid commit")
	}
	return g.run("show", "-s", "--format=%B", sha, "--")
}

// FileAt reads a tracked file without checking out or executing that revision.
func (g Git) FileAt(sha, path string) (string, error) {
	if !ValidSHA(sha) {
		return "", errors.New("invalid commit")
	}
	return g.run("show", sha+":"+path)
}

// MergeBase identifies the base of the changes introduced by a PR.
func (g Git) MergeBase(base, head string) (string, error) {
	if !ValidSHA(base) || !ValidSHA(head) {
		return "", errors.New("invalid commit")
	}
	out, err := g.run("merge-base", base, head)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !ValidSHA(sha) {
		return "", errors.New("merge base is not a commit")
	}
	return sha, nil
}
