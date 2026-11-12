package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
)

func commitSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestPreviewUsesBaseRulesWithoutExecutingPR(t *testing.T) {
	dir := fixtureRepo(t, "two-services", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--tool-version", "v0.1.4"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Shared = []config.Shared{{Paths: []string{"db/**"}, Units: []string{cfg.Units[0].ID, cfg.Units[1].ID}}}
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.FileName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "base rules")
	base := commitSHA(t, dir)
	// Proposed config cannot hide the PR's application changes.
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte("invalid: untrusted"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "db", "migrate", "<img onerror=alert(1)>")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db", "schema.rb"), []byte("schema"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "PR")
	head := commitSHA(t, dir)
	out.Reset()
	e.Getenv = func(key string) string {
		if key == "PR_TITLE" {
			return "batch [skip-deploy]"
		}
		return ""
	}
	if err := Run(e, []string{"preview", "--base", base, "--head", head}); err != nil {
		t.Fatal(err)
	}
	for _, unit := range cfg.Units {
		if !strings.Contains(out.String(), "<td>"+unit.ID+"</td><td>Would deploy after CI</td>") {
			t.Fatalf("missing unit impact: %s", out)
		}
	}
	for _, want := range []string{"Plan only. No deploy has run.", "base branch", "[skip-deploy]", "schema.rb", "&lt;img", config.FileName} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out.String(), "<img") {
		t.Fatal("unescaped PR path")
	}
}

func TestPreviewRejectsInvalidRevisions(t *testing.T) {
	e, _ := testEnv(t.TempDir())
	if err := Run(e, []string{"preview", "--base", "--help", "--head", strings.Repeat("a", 40)}); err == nil {
		t.Fatal("accepted invalid SHA")
	}
}
