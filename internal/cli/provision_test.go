package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/generate"
)

type call struct {
	name  string
	args  []string
	stdin string
}

type fakeRunner struct {
	calls           []call
	envExists       bool
	environmentJSON string
	policiesJSON    string
	secrets         string // JSON for gh secret list
	variables       string
	fingerprint     string
	piped           [][]string
}

func (f *fakeRunner) LookPath(string) error { return nil }

func (f *fakeRunner) Run(stdin io.Reader, name string, args ...string) ([]byte, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.calls = append(f.calls, call{name, args, in})
	joined := name + " " + strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "gh auth status"):
		return nil, nil
	case strings.HasPrefix(joined, "gh api repos/") && strings.HasSuffix(joined, "--silent") && !strings.Contains(joined, "-X"):
		if f.envExists {
			return nil, nil
		}
		return nil, errors.New("gh api: HTTP 404")
	case strings.HasPrefix(joined, "gh api repos/") && strings.Contains(joined, "/deployment-branch-policies"):
		if f.policiesJSON != "" {
			return []byte(f.policiesJSON), nil
		}
		return []byte(`{"total_count":1,"branch_policies":[{"name":"main","type":"branch"}]}`), nil
	case strings.HasPrefix(joined, "gh api repos/"):
		if f.environmentJSON != "" {
			return []byte(f.environmentJSON), nil
		}
		return []byte(`{"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}`), nil
	case strings.HasPrefix(joined, "gh api -X"):
		return nil, nil
	case strings.HasPrefix(joined, "gh secret list"):
		return []byte(f.secrets), nil
	case strings.HasPrefix(joined, "gh variable list"):
		return []byte(f.variables), nil
	case strings.HasPrefix(joined, "gh secret set"), strings.HasPrefix(joined, "gh variable set"):
		return nil, nil
	case name == "ssh-keygen" && args[0] == "-q":
		path := args[len(args)-1]
		_ = os.WriteFile(path, []byte("MOCK-KEY-FILE\nPRIVATEKEYMATERIAL\n"), 0o600)
		_ = os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAAPUBLIC kamal-autodeploy-ci\n"), 0o644)
		return nil, nil
	case name == "ssh-keyscan":
		return []byte("# 203.0.113.10:22 SSH-2.0-OpenSSH_9.6\n203.0.113.10 ssh-ed25519 AAAAHOSTKEY\n"), nil
	case name == "ssh-keygen" && args[0] == "-l":
		return []byte("256 " + f.fingerprint + " 203.0.113.10 (ED25519)\n"), nil
	}
	return nil, errors.New("unexpected command " + joined)
}

func (f *fakeRunner) Pipe(from, env, to []string) error {
	f.piped = append(f.piped, append(append([]string{}, from...), to...))
	return nil
}

func (f *fakeRunner) argv() string {
	var b strings.Builder
	for _, c := range f.calls {
		b.WriteString(c.name + " " + strings.Join(c.args, " ") + "\n")
	}
	return b.String()
}

func (f *fakeRunner) stdinFor(prefix string) (string, bool) {
	for _, c := range f.calls {
		if strings.HasPrefix(c.name+" "+strings.Join(c.args, " "), prefix) {
			return c.stdin, true
		}
	}
	return "", false
}

const hostFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func provisionRepo(t *testing.T) string {
	dir := fixtureRepo(t, "two-services", "https://github.com/acme/two-services.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--tool-version", "v0.1.0"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Local ignored secrets: one literal, one computed by a command (never evaluated).
	if err := os.WriteFile(filepath.Join(dir, ".kamal/secrets.rails"), []byte("FIXTURE_RAILS_SECRET='s3cr3t-value'\nKAMAL_REGISTRY_PASSWORD=$(op read op://x)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".kamal/secrets.edge"), []byte("FIXTURE_EDGE_SECRET=$(cat /etc/edge)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProvisionSecretsOnlyOnStdin(t *testing.T) {
	dir := provisionRepo(t)
	f := &fakeRunner{secrets: `[]`, variables: `[]`, fingerprint: hostFP}
	e, out := testEnv(dir)
	e.Runner, e.Piper = f, f
	err := Run(e, []string{"provision", "--yes", "--host-fingerprint", "203.0.113.10=" + hostFP})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	argv := f.argv()
	for _, secret := range []string{"s3cr3t-value", "PRIVATEKEYMATERIAL"} {
		if strings.Contains(argv, secret) || strings.Contains(out.String(), secret) {
			t.Errorf("%s leaked into argv or output", secret)
		}
	}
	if in, ok := f.stdinFor("gh secret set " + generate.SSHKeySecret + " --env production-rails"); !ok || !strings.Contains(in, "PRIVATEKEYMATERIAL") {
		t.Error("the CI private key must be set through stdin")
	}
	if in, ok := f.stdinFor("gh secret set FIXTURE_RAILS_SECRET --env production-rails"); !ok || in != "s3cr3t-value" {
		t.Errorf("literal secret must be set through stdin, got %q", in)
	}
	if _, ok := f.stdinFor("gh secret set FIXTURE_EDGE_SECRET"); ok {
		t.Error("a value computed by a command must never be evaluated or stored")
	}
	if !strings.Contains(out.String(), "FIXTURE_EDGE_SECRET: .kamal/secrets.edge computes it") {
		t.Errorf("computed secret must be reported for manual setup:\n%s", out)
	}
	if in, ok := f.stdinFor("gh variable set " + generate.KnownHostsVar + " --env production-edge"); !ok || in != "203.0.113.10 ssh-ed25519 AAAAHOSTKEY\n" {
		t.Errorf("pinned known_hosts = %q", in)
	}
	if strings.Contains(argv, "StrictHostKeyChecking=no") {
		t.Error("host key checking must never be disabled")
	}
	if !strings.Contains(out.String(), "ssh-ed25519 AAAAPUBLIC") {
		t.Error("the public key must be shown for the user to authorize")
	}
	if !strings.Contains(argv, "gh api -X PUT repos/acme/two-services/environments/production-rails --input - --silent") {
		t.Errorf("missing environment must be created:\n%s", argv)
	}
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "kamal-autodeploy-key-*"))
	if len(matches) > 0 {
		t.Errorf("temporary key directories left behind: %v", matches)
	}
}

func TestProvisionHostKeyMismatchPinsNothing(t *testing.T) {
	dir := provisionRepo(t)
	f := &fakeRunner{envExists: true, secrets: `[{"name":"KAMAL_AUTODEPLOY_SSH_KEY"}]`, variables: `[]`, fingerprint: "SHA256:attacker"}
	e, out := testEnv(dir)
	e.Runner, e.Piper = f, f
	err := Run(e, []string{"provision", "--yes", "--unit", "rails", "--host-fingerprint", "203.0.113.10=" + hostFP})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch must stop: %v\n%s", err, out)
	}
	if _, ok := f.stdinFor("gh variable set"); ok {
		t.Error("nothing may be pinned after a mismatch")
	}
	// Without a verified fingerprint and no terminal, nothing is pinned either.
	f2 := &fakeRunner{envExists: true, secrets: `[]`, variables: `[]`, fingerprint: hostFP}
	e.Runner, e.Piper = f2, f2
	if err := Run(e, []string{"provision", "--yes", "--unit", "rails"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f2.stdinFor("gh variable set " + generate.KnownHostsVar); ok {
		t.Error("ssh-keyscan alone is not verification")
	}
}

func TestProvisionNeverOverwritesWithoutAsking(t *testing.T) {
	dir := provisionRepo(t)
	f := &fakeRunner{envExists: true, secrets: `[{"name":"KAMAL_AUTODEPLOY_SSH_KEY"},{"name":"FIXTURE_RAILS_SECRET"}]`, variables: `[{"name":"KAMAL_AUTODEPLOY_KNOWN_HOSTS","value":"203.0.113.10 ssh-ed25519 AAAAHOSTKEY"}]`, fingerprint: hostFP}
	e, out := testEnv(dir)
	e.Runner, e.Piper = f, f
	if err := Run(e, []string{"provision", "--yes", "--overwrite", "--unit", "rails"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := f.stdinFor("gh secret set"); ok {
		t.Errorf("--yes --overwrite without a terminal must not replace existing secrets:\n%s", f.argv())
	}
}

func TestProvisionRefusesTrackedImport(t *testing.T) {
	dir := provisionRepo(t)
	f := &fakeRunner{envExists: true, secrets: `[{"name":"KAMAL_AUTODEPLOY_SSH_KEY"}]`, variables: `[{"name":"KAMAL_AUTODEPLOY_KNOWN_HOSTS","value":"203.0.113.10 ssh-ed25519 AAAAHOSTKEY"}]`}
	e, _ := testEnv(dir)
	e.Runner, e.Piper = f, f
	if err := Run(e, []string{"provision", "--yes", "--unit", "rails", "--import", ".kamal/secrets.rails.example"}); err == nil || !strings.Contains(err.Error(), "tracked") {
		t.Fatalf("tracked import must be refused: %v", err)
	}
}

func TestProvisionUpstateIsOptInOnly(t *testing.T) {
	dir := provisionRepo(t)
	os.MkdirAll(filepath.Join(dir, ".upstate"), 0o755)
	os.WriteFile(filepath.Join(dir, ".upstate", "fixture_rails.default.json"), []byte(`{"environment_public_id":"env_abc123","origin":"https://upstate.cloud","service":"fixture_rails","destination":"default"}`), 0o644)
	f := &fakeRunner{envExists: true, secrets: `[{"name":"KAMAL_AUTODEPLOY_SSH_KEY"},{"name":"FIXTURE_RAILS_SECRET"}]`, variables: `[{"name":"KAMAL_AUTODEPLOY_KNOWN_HOSTS","value":"203.0.113.10 ssh-ed25519 AAAAHOSTKEY"}]`}
	e, out := testEnv(dir)
	e.Runner, e.Piper = f, f
	if err := Run(e, []string{"provision", "--yes", "--unit", "rails"}); err != nil {
		t.Fatal(err)
	}
	if len(f.piped) != 0 || !strings.Contains(out.String(), "--yes never opts into Upstate") {
		t.Errorf("--yes must not store the Upstate token: %v\n%s", f.piped, out)
	}
	// Interactive yes pipes upstate's output straight into gh's stdin.
	e.Interactive, e.In = true, strings.NewReader("y\n")
	out.Reset()
	if err := Run(e, []string{"provision", "--unit", "rails"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.piped) != 1 || strings.Join(f.piped[0], " ") != "upstate credential hook env_abc123 gh secret set UPSTATE_HOOK_TOKEN --env production-rails --repo acme/two-services" {
		t.Errorf("piped %v", f.piped)
	}
	if in, ok := f.stdinFor("gh variable set UPSTATE_HOOK_ENVIRONMENT --env production-rails"); !ok || in != "env_abc123" {
		t.Errorf("UPSTATE_HOOK_ENVIRONMENT = %q", in)
	}
	// --no-upstate skips the whole step.
	f.piped = nil
	e.In = strings.NewReader("y\n")
	if err := Run(e, []string{"provision", "--unit", "rails", "--no-upstate"}); err != nil || len(f.piped) != 0 {
		t.Errorf("--no-upstate: %v %v", err, f.piped)
	}
}

func TestDotenvParsingNeverEvaluates(t *testing.T) {
	got := parseDotenv([]byte("A=plain # comment\nB='single $(x)'\nC=\"double\"\nD=$(cat key)\nE=$OTHER\nexport F=ok\nG=\"esc\\n\"\n"))
	cases := map[string]string{"A": "plain", "B": "single $(x)", "C": "double", "F": "ok"}
	for k, want := range cases {
		if !got[k].literal || string(got[k].value) != want {
			t.Errorf("%s = %+v", k, got[k])
		}
	}
	for _, k := range []string{"D", "E", "G"} {
		if got[k].literal {
			t.Errorf("%s must not be imported as a literal", k)
		}
	}
}

func TestDoctorReportsMissingSetup(t *testing.T) {
	dir := provisionRepo(t)
	f := &fakeRunner{secrets: `[]`, variables: `[]`}
	e, out := testEnv(dir)
	e.Runner, e.Piper = f, f
	err := Run(e, []string{"doctor"})
	if err == nil {
		t.Fatalf("doctor must fail without environments:\n%s", out)
	}
	if !strings.Contains(out.String(), "GitHub environment production-rails does not exist") {
		t.Errorf("%s", out)
	}
	// --remote dispatches only with confirmation, through gh workflow run with JSON inputs.
	f.envExists = true
	out.Reset()
	_ = Run(e, []string{"doctor", "--remote", "--unit", "edge"})
	if _, ok := f.stdinFor("gh workflow run"); ok {
		t.Error("--remote must not dispatch without a yes")
	}
}

func TestProvisionRejectsUnsafeEnvironmentBeforeSecrets(t *testing.T) {
	for _, tc := range []struct{ name, environment, policies string }{
		{"unrestricted", `{}`, ""},
		{"protected branches", `{"deployment_branch_policy":{"protected_branches":true,"custom_branch_policies":false}}`, ""},
		{"wildcard", "", `{"total_count":1,"branch_policies":[{"name":"*","type":"branch"}]}`},
		{"tag", "", `{"total_count":1,"branch_policies":[{"name":"main","type":"tag"}]}`},
		{"extra policy", "", `{"total_count":2,"branch_policies":[{"name":"main","type":"branch"},{"name":"other","type":"branch"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := provisionRepo(t)
			f := &fakeRunner{envExists: true, environmentJSON: tc.environment, policiesJSON: tc.policies}
			e, _ := testEnv(dir)
			e.Runner, e.Piper = f, f
			if err := Run(e, []string{"provision", "--yes", "--unit", "rails"}); err == nil {
				t.Fatal("unsafe environment accepted")
			}
			if strings.Contains(f.argv(), "gh secret") || strings.Contains(f.argv(), "gh api -X PUT") {
				t.Fatal("unsafe environment mutated or secrets accessed")
			}
		})
	}
}
