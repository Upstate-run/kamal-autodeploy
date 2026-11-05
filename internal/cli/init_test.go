package cli

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/generate"
)

var update = flag.Bool("update", false, "rewrite the examples/ golden files")

// fixtureRepo copies a fixture tree into a fresh git repository with a github.com origin.
func fixtureRepo(t *testing.T, fixture, remote string) string {
	t.Helper()
	src := filepath.Join("..", "..", "fixtures", fixture)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, _ := d.Info()
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	git(t, dst, "init", "-q", "-b", "main")
	git(t, dst, "remote", "add", "origin", remote)
	git(t, dst, "add", "-A")
	git(t, dst, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "fixture")
	return dst
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func testEnv(dir string) (*Env, *bytes.Buffer) {
	out := &bytes.Buffer{}
	r := osRunner{dir: dir}
	return &Env{Dir: dir, In: strings.NewReader(""), Out: out, Err: out, Runner: r, Piper: r, Getenv: func(string) string { return "" }}, out
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/cli -update)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the generated output (run go test ./internal/cli -update to accept):\n%s", path, unifiedDiff(path, want, got))
	}
}

func TestInitSimpleSingleConfig(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--tool-version", "v0.1.0"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Units[0]
	if len(cfg.Units) != 1 || u.ID != "shop" || u.Command != "bin/kamal" || u.Kamal != config.KamalBundler || u.Environment != "production" {
		t.Errorf("unit %+v", u)
	}
	if u.WriteSecretsFile || u.RegistryPassword != config.RegistryGitHubToken || strings.Join(u.Secrets, ",") != "RAILS_MASTER_KEY" {
		t.Errorf("secrets: tracked .kamal/secrets is used as is, GHCR uses GITHUB_TOKEN: %+v", u)
	}
	if strings.Join(u.SSH.Hosts, ",") != "198.51.100.20" || strings.Join(u.Requires, ",") != "CI" {
		t.Errorf("hosts %v requires %v", u.SSH.Hosts, u.Requires)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, config.FileName))
	golden(t, "../../examples/simple/.kamal-autodeploy.yml", raw)
	wf, _ := os.ReadFile(filepath.Join(dir, config.WorkflowPath))
	golden(t, "../../examples/simple/kamal-autodeploy.yml", wf)

	// Idempotent: a second run writes nothing.
	out.Reset()
	if err := Run(e, []string{"init", "--yes"}); err != nil || !strings.Contains(out.String(), "up to date") {
		t.Errorf("second init: %v\n%s", err, out)
	}
}

func TestInitTwoConfigTwoService(t *testing.T) {
	dir := fixtureRepo(t, "two-services", "https://github.com/acme/two-services.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"plan", "--tool-version", "v0.1.0"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	plan := out.String()
	for _, want := range []string{"rails  ->  GitHub environment production-rails", "edge  ->  GitHub environment production-edge",
		"bin/kamal-rails deploy --version=<commit>", "services/edge/ is inside rails's build context", "Dry run: nothing was written"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		t.Fatal("plan must not write")
	}
	out.Reset()
	if err := Run(e, []string{"init", "--yes", "--tool-version", "v0.1.0"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	rails, _ := cfg.Unit("rails")
	edge, _ := cfg.Unit("edge")
	if rails.Command != "bin/kamal-rails" || len(rails.Args) != 0 || edge.Command != "bin/kamal-edge" {
		t.Errorf("wrappers must be preserved exactly: %+v %+v", rails, edge)
	}
	if rails.Lock != edge.Lock || strings.Join(edge.After, ",") != "rails" || cfg.Units[0].ID != "rails" {
		t.Errorf("shared host must serialize with rails first: rails=%+v edge=%+v", rails, edge)
	}
	if !rails.WriteSecretsFile || rails.SecretsFile != ".kamal/secrets.rails" || strings.Join(rails.Secrets, ",") != "FIXTURE_RAILS_SECRET" {
		t.Errorf("rails secrets %+v", rails)
	}
	if strings.Join(edge.Requires, ",") != "CI,Edge" || strings.Join(rails.Requires, ",") != "CI" {
		t.Errorf("requires rails=%v edge=%v", rails.Requires, edge.Requires)
	}
	if !config.Match(rails.Paths, "services/edge/public/index.html") {
		t.Error("conservative default: rails' root context includes services/edge until a person excludes it")
	}
	if config.Match(rails.Paths, "config/deploy.edge.yml") || config.Match(rails.Paths, "bin/kamal-edge") || !config.Match(rails.Paths, "config/deploy.rails.yml") || !config.Match(rails.Paths, "Dockerfile") {
		t.Errorf("rails paths %v", rails.Paths)
	}
	if config.Match(rails.Paths, ".github/workflows/ci.yml") {
		t.Error(".dockerignore'd paths are not rails-owned")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, config.FileName))
	golden(t, "../../examples/two-config/.kamal-autodeploy.yml", raw)
	wf, _ := os.ReadFile(filepath.Join(dir, config.WorkflowPath))
	golden(t, "../../examples/two-config/kamal-autodeploy.yml", wf)

	// The reviewed config the acceptance runbook applies: Edge's tree is not in the Rails
	// image, and the schema deploys both (Rails first).
	for i := range cfg.Units {
		if cfg.Units[i].ID == "rails" {
			cfg.Units[i].Paths = append(cfg.Units[i].Paths, "!services/edge/**")
		}
	}
	cfg.Shared = append(cfg.Shared, config.Shared{Paths: []string{"db/schema.sql", "db/migrate/**"}, Units: []string{"rails", "edge"}})
	reviewed, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "../../examples/two-config/reviewed/.kamal-autodeploy.yml", reviewed)
	if err := os.WriteFile(filepath.Join(dir, config.FileName), reviewed, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"init", "--yes"}); err != nil {
		t.Fatalf("regenerate from the reviewed config: %v\n%s", err, out)
	}
	// Paths and shared rules are read at run time from the config; the workflow is unchanged.
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("path rules must not change the workflow:\n%s", out)
	}
}

func TestInitRefusesHandWrittenWorkflows(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	wfPath := filepath.Join(dir, config.WorkflowPath)
	if err := os.WriteFile(wfPath, []byte("name: mine\non: push\njobs: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(e, []string{"init", "--yes", "--tool-version", "v0.1.0"}); err == nil || !strings.Contains(err.Error(), "not generated by kamal-autodeploy") {
		t.Fatalf("a foreign workflow must never be overwritten: %v", err)
	}
	if err := Run(e, []string{"init", "--yes", "--force", "--tool-version", "v0.1.0"}); err == nil {
		t.Fatal("--force must not overwrite a foreign workflow either")
	}
	if raw, _ := os.ReadFile(wfPath); string(raw) != "name: mine\non: push\njobs: {}\n" {
		t.Fatal("foreign workflow changed")
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		t.Fatal("nothing must be written when the workflow is refused")
	}
	// Generated, then hand-edited: refused unless --force, with the diff shown.
	os.Remove(wfPath)
	if err := Run(e, []string{"init", "--yes", "--tool-version", "v0.1.0"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(wfPath)
	edited := strings.Replace(string(raw), "timeout-minutes: 60", "timeout-minutes: 90", 1)
	os.WriteFile(wfPath, []byte(edited), 0o644)
	cfgRaw, _ := os.ReadFile(filepath.Join(dir, config.FileName))
	os.WriteFile(filepath.Join(dir, config.FileName), []byte(strings.Replace(string(cfgRaw), "unclassified: deploy", "unclassified: stop", 1)), 0o644)
	out.Reset()
	if err := Run(e, []string{"init", "--yes"}); err == nil || !strings.Contains(err.Error(), "edited by hand") {
		t.Fatalf("hand-edited workflow must be refused: %v", err)
	}
	if got, _ := os.ReadFile(wfPath); string(got) != edited {
		t.Fatal("hand-edited workflow changed")
	}
	if err := Run(e, []string{"init", "--yes", "--force"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(wfPath); generate.Classify(got, got) != generate.Current || strings.Contains(string(got), "timeout-minutes: 90") {
		t.Fatal("--force replaces a hand-edited generated workflow")
	}
}

func TestInitNonInteractiveNeedsYes(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, _ := testEnv(dir)
	if err := Run(e, []string{"init", "--tool-version", "v0.1.0"}); err == nil || !strings.Contains(err.Error(), "confirmation") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		t.Fatal("wrote without confirmation")
	}
}
