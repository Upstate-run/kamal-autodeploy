package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/discover"
	"gopkg.in/yaml.v3"
)

type readOnlyRunner struct {
	calls        []string
	packageError bool
}

func (r *readOnlyRunner) LookPath(string) error { return nil }
func (r *readOnlyRunner) Run(in io.Reader, name string, args ...string) ([]byte, error) {
	c := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, c)
	if in != nil {
		return nil, errors.New("unexpected stdin")
	}
	switch {
	case strings.HasPrefix(c, "gh auth status"):
		return nil, nil
	case strings.HasPrefix(c, "gh repo view"):
		return []byte("main\n"), nil
	case strings.HasPrefix(c, "gh api users/acme/packages/"):
		if r.packageError {
			return nil, errors.New("HTTP 403: sensitive diagnostic must not be echoed")
		}
		return []byte(`{"repository":{"full_name":"acme/shop"}}`), nil
	case c == "gh api users/acme":
		return []byte(`{"type":"User"}`), nil
	case strings.HasPrefix(c, "gh api repos/acme/shop/environments/"):
		return nil, errors.New("HTTP 404")
	}
	return nil, errors.New("unexpected or mutating command: " + c)
}

func TestReadyBeforeInitIsReadOnlyAndOffline(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	r := &readOnlyRunner{}
	e.Runner = r
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(r.calls) != 0 {
		t.Fatalf("offline ran commands: %v", r.calls)
	}
	for _, p := range []string{config.FileName, config.WorkflowPath} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("wrote %s", p)
		}
	}
	for _, want := range []string{"NEXT: Review and prepare", "kamal-autodeploy init --direct-kamal", "offline:", "runner", "Manage Actions access"} {
		if !strings.Contains(strings.ToLower(out.String()), strings.ToLower(want)) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Error("redirected output contains ANSI")
	}
}

func TestReadySeparatesMissingGitHubSetupFromRepositoryPreparation(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	r := &readOnlyRunner{packageError: true}
	e.Runner = r
	if err := Run(e, []string{"ready", "--direct-kamal"}); err == nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"GitHub environment production is missing or inaccessible", "does not prove Actions access is missing", "add access only if missing", "NEXT: Review and prepare"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "sensitive diagnostic") {
		t.Error("echoed API diagnostic")
	}
	for _, c := range r.calls {
		if !strings.HasPrefix(c, "gh api ") && !strings.HasPrefix(c, "gh auth status") && !strings.HasPrefix(c, "gh repo view") {
			t.Errorf("unexpected command %s", c)
		}
	}
}

func TestReadyFlagsTrackedSecretCommandsWithoutExecutingOrPrintingValues(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	marker := filepath.Join(dir, "should-not-exist")
	raw := "KAMAL_REGISTRY_PASSWORD=$KAMAL_REGISTRY_PASSWORD\nRAILS_MASTER_KEY=$(touch " + marker + "; echo VERY_PRIVATE_VALUE)\n"
	if err := os.WriteFile(filepath.Join(dir, ".kamal", "secrets"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "tracked secrets entry RAILS_MASTER_KEY") {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), "VERY_PRIVATE_VALUE") || strings.Contains(out.String(), marker) {
		t.Fatal("leaked secret command")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("executed secret command")
	}
}

func TestReadyBlocksUnsupportedWrapperAndOffersExplicitDirectMode(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	if err := os.WriteFile(filepath.Join(dir, "bin", "kamal"), []byte("#!/bin/sh\ncd /opt/app\nexec kamal \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	if err := Run(e, []string{"ready", "--offline"}); err == nil {
		t.Fatal("accepted unsupported routing")
	}
	if !strings.Contains(out.String(), "init --direct-kamal") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err != nil {
		t.Fatalf("explicit direct mode: %v\n%s", err, out)
	}
}

func TestRegistryLinkedMetadataIsNotProofOfActionsAccess(t *testing.T) {
	e, out := testEnv(t.TempDir())
	r := &readOnlyRunner{}
	e.Runner = r
	cfg := &config.Config{Units: []config.Unit{{ID: "shop", Config: "config/deploy.yml", RegistryPassword: config.RegistryGitHubToken}}}
	report := &discover.Report{Units: []discover.UnitInfo{{Base: "config/deploy.yml", Image: "acme/shop"}}}
	registryChecks(e, cfg, report, "acme", "shop", true)
	for _, want := range []string{"linked", "not proven", "https://github.com/users/acme/packages/container/shop/settings", "unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
}

func TestOutputColorAndPlainLabels(t *testing.T) {
	e, out := testEnv(t.TempDir())
	e.Color = true
	e.checks([]check{{"ok", "verified"}, {"fail", "blocked"}})
	if !strings.Contains(out.String(), "\x1b[32m") || !strings.Contains(out.String(), "\x1b[31m") {
		t.Fatal("missing status colors")
	}
	for _, key := range []string{"NO_COLOR", "TERM"} {
		out.Reset()
		e.Getenv = func(k string) string {
			if k == key {
				if key == "TERM" {
					return "dumb"
				}
				return "1"
			}
			return ""
		}
		e.next("Fix the failure", "Run doctor")
		if strings.Contains(out.String(), "\x1b[") || !strings.Contains(out.String(), "NEXT: Fix the failure") {
			t.Fatal(out.String())
		}
	}
}

func TestDoctorDoesNotDispatchWhenSetupFailed(t *testing.T) {
	dir := provisionRepo(t)
	e, _ := testEnv(dir)
	r := &fakeRunner{secrets: `[]`, variables: `[]`}
	e.Runner = r
	if err := Run(e, []string{"doctor", "--remote", "--unit", "rails", "--yes"}); err == nil {
		t.Fatal("expected failed setup")
	}
	if _, ok := r.stdinFor("gh workflow run"); ok {
		t.Fatal("dispatched after failing setup checks")
	}
}

func TestReadyIncludesTrackedCommonSecretsAndHooks(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	common := filepath.Join(dir, ".kamal", "secrets-common")
	if err := os.WriteFile(common, []byte("EXTRA=$(echo HIDDEN_SECRET)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-f", ".kamal/secrets-common")
	hooks := filepath.Join(dir, ".kamal", "hooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-deploy"), []byte("#!/bin/sh\nexit 99\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{".kamal/secrets-common: tracked secrets entry EXTRA", "hook .kamal/hooks/post-deploy", "local file/command dependencies"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out.String(), "HIDDEN_SECRET") {
		t.Fatal("leaked common secret")
	}
}

func TestReadyUsesReviewedConfigAndReportsMissingSelectedConfig(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "using reviewed") || !strings.Contains(out.String(), "activation manual") {
		t.Fatal(out.String())
	}
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err == nil {
		t.Fatal("ignored reviewed config")
	}
	cfgPath := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Another valid config still exists, so discovery succeeds but cannot match the reviewed selection.
	if err := os.WriteFile(cfgPath, []byte(strings.ReplaceAll(string(raw), "config/deploy.yml", "config/absent.yml")), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err == nil {
		t.Fatal("accepted undiscoverable selected config")
	}
	if !strings.Contains(out.String(), "selected config/destination is not statically discoverable") {
		t.Fatal(out.String())
	}
}

func TestTerminalDetectionRejectsCharacterDevicesThatAreNotTerminals(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("a character device is not necessarily a terminal")
	}
}

func TestReadyChecksReviewedDeployBranchAndChangedCIGates(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	workflow := filepath.Join(dir, ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflow, []byte(strings.ReplaceAll(string(raw), "main", "release")), 0644); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--branch", "release", "--tool-version", "v0.1.4"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err != nil {
		t.Fatalf("did not use reviewed deploy branch: %v\n%s", err, out)
	}
	if err := os.WriteFile(workflow, []byte("name: Renamed\non: workflow_dispatch\njobs: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err == nil {
		t.Fatal("accepted removed push CI gate")
	}
	if !strings.Contains(out.String(), "missing, renamed or not statically known to run on push to release") {
		t.Fatal(out.String())
	}
}

func TestReadyRejectsWrapperArgumentDrift(t *testing.T) {
	for _, args := range [][]string{{"-c", "config/other.yml"}, {"-d", "other"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--tool-version", "v0.1.4"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, config.FileName)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Units[0].Args = args
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := Run(e, []string{"ready", "--offline"}); err == nil {
				t.Fatal("accepted wrapper argument drift")
			}
			if !strings.Contains(out.String(), "wrapper command/arguments differ") {
				t.Fatal(out.String())
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(raw) {
				t.Fatal("changed reviewed configuration")
			}
		})
	}
}

func TestReadyRejectsCurrentTargetsOutsideReviewedSettings(t *testing.T) {
	for _, kind := range []string{"host", "port"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, config.FileName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			kamal := filepath.Join(dir, "config", "deploy.yml")
			raw, err := os.ReadFile(kamal)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "host" {
				raw = []byte(strings.ReplaceAll(string(raw), "198.51.100.20", "198.51.100.21"))
			} else {
				raw = append(raw, []byte("\nssh:\n  port: 2222\n")...)
			}
			if err := os.WriteFile(kamal, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := Run(e, []string{"ready", "--offline"}); err == nil {
				t.Fatal("accepted targets outside reviewed settings")
			}
			if !strings.Contains(out.String(), "current Kamal hosts/port are not covered") {
				t.Fatal(out.String())
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("changed reviewed configuration")
			}
		})
	}
}

func TestReadyRejectsEmptyStaticTargetsBeforeInit(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	path := filepath.Join(dir, "config", "deploy.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kamal map[string]any
	if err := yaml.Unmarshal(raw, &kamal); err != nil {
		t.Fatal(err)
	}
	kamal["servers"] = []any{}
	raw, err = yaml.Marshal(kamal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err == nil {
		t.Fatal("accepted empty static server list")
	}
	if !strings.Contains(out.String(), "selected Kamal config has no SSH targets") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); !os.IsNotExist(err) {
		t.Fatal("wrote proposal")
	}
}

func TestReadyRejectsDuplicateDiscoveredCINames(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	raw, err := os.ReadFile(filepath.Join(dir, ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github/workflows/duplicate.yml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err == nil {
		t.Fatal("accepted duplicate gate names")
	}
	if !strings.Contains(out.String(), `CI workflow "CI" is listed twice`) {
		t.Fatal(out.String())
	}
}

func TestReadyRejectsSSHUserAndRequiredSecretDrift(t *testing.T) {
	for _, kind := range []string{"user", "secret"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config/deploy.yml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "current Kamal SSH user differs"
			if kind == "user" {
				raw = append(raw, []byte("\nssh:\n  user: deploy\n")...)
			} else {
				raw = []byte(strings.ReplaceAll(string(raw), "    - RAILS_MASTER_KEY", "    - RAILS_MASTER_KEY\n    - DATABASE_PASSWORD"))
				f, err := os.OpenFile(filepath.Join(dir, ".kamal/secrets"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("\nDATABASE_PASSWORD=$DATABASE_PASSWORD\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
				want = "newly discovered secret DATABASE_PASSWORD"
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := Run(e, []string{"ready", "--offline"}); err == nil {
				t.Fatal("accepted drift")
			}
			if !strings.Contains(out.String(), want) {
				t.Fatal(out.String())
			}
		})
	}
}

func TestReadySharesSecretsFileTrackingContract(t *testing.T) {
	for _, kind := range []string{"tracked-generated", "untracked-required", "not-ignored"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, config.FileName)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "is tracked but the workflow would write it"
			switch kind {
			case "tracked-generated":
				cfg.Units[0].WriteSecretsFile = true
			case "untracked-required":
				git(t, dir, "rm", "--cached", ".kamal/secrets")
				want = "is not tracked, so CI has no secrets file"
			case "not-ignored":
				git(t, dir, "rm", "--cached", ".kamal/secrets")
				cfg.Units[0].WriteSecretsFile = true
				git(t, dir, "add", "-f", ".gitignore")
				if err := os.WriteFile(filepath.Join(dir, ".gitignore"), nil, 0644); err != nil {
					t.Fatal(err)
				}
				want = "is not ignored by git"
			}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := Run(e, []string{"ready", "--offline"}); err == nil {
				t.Fatal("accepted incompatible file tracking")
			}
			if !strings.Contains(out.String(), want) {
				t.Fatal(out.String())
			}
		})
	}
}

func TestReadyHonorsReviewedTokenRegistryWithDynamicUsername(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config/deploy.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "username: acme", `username: <%= ENV.fetch("REGISTRY_USER") %>`))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "newly discovered secret KAMAL_REGISTRY_PASSWORD") {
		t.Fatal(out.String())
	}
}

func TestReadyRetainsSelectedUnsupportedSecretDiagnostics(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config/deploy.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "    - RAILS_MASTER_KEY", "    - RAILS_MASTER_KEY\n    - KAMAL_AUTODEPLOY_API_KEY"))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err == nil {
		t.Fatal("accepted unusable required secret")
	}
	if !strings.Contains(out.String(), "secret KAMAL_AUTODEPLOY_API_KEY has a name the deploy workflow uses itself") {
		t.Fatal(out.String())
	}
}

func TestReadyDistinguishesChangedAndUnresolvedSecretsPaths(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		t.Run(fmt.Sprint(dynamic), func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			git(t, dir, "rm", "--cached", ".kamal/secrets")
			f, err := os.OpenFile(filepath.Join(dir, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("\n.kamal/secrets\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(dir, config.FileName)
			before, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config/deploy.yml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			value := ".kamal/other-secrets"
			if dynamic {
				value = `<%= ENV.fetch("SECRETS_PATH") %>`
			}
			raw = append(raw, []byte("\nsecrets_path: "+value+"\n")...)
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			err = Run(e, []string{"ready", "--offline"})
			if dynamic {
				if err != nil || !strings.Contains(out.String(), "actual secrets file path remains unverified") || strings.Contains(out.String(), "current Kamal secrets_path differs") {
					t.Fatalf("%v\n%s", err, out)
				}
			} else if err == nil || !strings.Contains(out.String(), "current Kamal secrets_path differs") {
				t.Fatalf("%v\n%s", err, out)
			}
			after, err := os.ReadFile(cfgPath)
			if err != nil || string(after) != string(before) {
				t.Fatal("changed reviewed plan")
			}
		})
	}
}

func TestReadyChecksReviewedTokenRegistryServerCompatibility(t *testing.T) {
	for _, kind := range []string{"docker-hub", "implicit-docker-hub", "unresolved", "ghcr"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config/deploy.yml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "docker-hub":
				raw = []byte(strings.ReplaceAll(string(raw), "server: ghcr.io", "server: docker.io"))
			case "implicit-docker-hub":
				raw = []byte(strings.ReplaceAll(string(raw), "  server: ghcr.io\n", ""))
			case "unresolved":
				raw = []byte(strings.ReplaceAll(string(raw), "server: ghcr.io", `server: <%= ENV.fetch("REGISTRY_SERVER") %>`))
			case "ghcr":
				raw = []byte(strings.ReplaceAll(string(raw), "username: acme", `username: <%= ENV.fetch("REGISTRY_USER") %>`))
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(dir, config.FileName)
			before, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			out.Reset()
			err = Run(e, []string{"ready", "--offline"})
			if kind == "docker-hub" || kind == "implicit-docker-hub" {
				if err == nil || !strings.Contains(out.String(), "current registry server is incompatible") {
					t.Fatalf("%v\n%s", err, out)
				}
			} else {
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if kind == "unresolved" && !strings.Contains(out.String(), "compatibility with the reviewed registry authentication policy remains unverified") {
					t.Fatal(out.String())
				}
			}
			after, err := os.ReadFile(cfgPath)
			if err != nil || string(after) != string(before) {
				t.Fatal("changed reviewed plan")
			}
		})
	}
}

func TestReadyUnresolvedSSHUserIsNotAConfirmedMismatch(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, config.FileName)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Units[0].SSH.User = "deploy"
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config/deploy.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\nssh:\n  user: <%= ENV.fetch(\"SSH_USER\") %>\n")...)
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "SSH user is unresolved") || strings.Contains(out.String(), "SSH user differs") {
		t.Fatal(out.String())
	}
}

func TestReadyUsesProposalValidatorForInvalidCIName(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	path := filepath.Join(dir, ".github/workflows/ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "name: CI", "name: CI$invalid"))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	if err := Run(e, []string{"ready", "--offline", "--direct-kamal"}); err == nil {
		t.Fatal("accepted invalid proposal")
	}
	if !strings.Contains(out.String(), "proposed config is not valid yet") {
		t.Fatal(out.String())
	}
}

func TestReadyRequiresNewRegistryPasswordForReviewedNoAuthPolicy(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.4"}); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, config.FileName)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Units[0].RegistryPassword = config.RegistryNone
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config/deploy.yml")
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "server: ghcr.io", "server: docker.io"))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err == nil {
		t.Fatal("missed required registry password")
	}
	if !strings.Contains(out.String(), "newly discovered secret KAMAL_REGISTRY_PASSWORD") {
		t.Fatal(out.String())
	}
}

func TestReadyPrivatePeerNeedsExplicitTailscale(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.8"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config/deploy.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "198.51.100.20", "100.101.102.103"))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Units[0].SSH.Hosts = []string{"100.101.102.103"}
	for _, enabled := range []bool{false, true} {
		if enabled {
			cfg.Units[0].Tailscale = &config.Tailscale{Tags: []string{"tag:deploy"}}
		}
		data, err := cfg.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, config.FileName), data, 0644); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		err = Run(e, []string{"ready", "--offline"})
		if (err == nil) != enabled {
			t.Fatalf("Tailscale enabled=%v: %v\n%s", enabled, err, out)
		}
	}
}
