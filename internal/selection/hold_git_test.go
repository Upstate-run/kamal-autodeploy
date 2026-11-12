package selection

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
)

// Exercise Git's real first-parent traversal, including markers hidden in a
// merged side branch and markers preserved by squash/rebase-style commits.
func TestHoldFirstParentTopology(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "--allow-empty", "-qm", "baseline")
	git("checkout", "-qb", "side")
	git("commit", "--allow-empty", "-qm", "[skip-deploy] side branch")
	git("checkout", "-q", "main")
	git("merge", "--no-ff", "-qm", "unmarked merge", "side")
	repo := gitx.Git{Dir: dir}
	candidates, err := repo.FirstParents("refs/remotes/origin/main", MaxCandidates)
	if err == nil {
		t.Fatal("unexpected remote ref")
	}
	candidates, err = repo.FirstParents(git("rev-parse", "HEAD"), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	eligible, hold, err := automaticCandidates(repo, candidates)
	if err != nil || hold != "" || len(eligible) != 2 {
		t.Fatalf("side marker leaked: %v %s %v", eligible, hold, err)
	}
	git("commit", "--allow-empty", "-qm", "squashed PR\n\n[skip-deploy]")
	marker := git("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-qm", "unmarked release")
	candidates, err = repo.FirstParents(git("rev-parse", "HEAD"), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	eligible, hold, err = automaticCandidates(repo, candidates)
	if err != nil || hold != marker || len(eligible) != 1 {
		t.Fatalf("release boundary: %v %s %v", eligible, hold, err)
	}
}
