package config

import (
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		patterns []string
		file     string
		want     bool
	}{
		{[]string{"**"}, "a/b/c.rb", true},
		{[]string{"app/**"}, "app/models/x.rb", true},
		{[]string{"app/**"}, "apps/x.rb", false},
		{[]string{"*.md"}, "README.md", true},
		{[]string{"*.md"}, "docs/x.md", false},
		{[]string{"**/*.md"}, "docs/x.md", true},
		{[]string{"**", "!services/edge/**"}, "services/edge/a.ex", false},
		{[]string{"**", "!services/edge/**", "services/edge/shared.rb"}, "services/edge/shared.rb", true},
		{[]string{"config/deploy*.yml"}, "config/deploy.rails.yml", true},
	}
	for _, c := range cases {
		if got := Match(c.patterns, c.file); got != c.want {
			t.Errorf("Match(%v, %q) = %v", c.patterns, c.file, got)
		}
	}
	for _, bad := range []string{"", "/abs", "../up", "a//b", "a\\b", "[", "a/./b"} {
		if ValidPattern(bad) == nil {
			t.Errorf("%q must be invalid", bad)
		}
	}
}

const valid = `version: 1
branch: main
tool_version: v0.1.0
ci:
  - name: CI
    path: .github/workflows/ci.yml
unclassified: deploy
units:
  - id: web
    environment: production
    command: bin/kamal
    config: config/deploy.yml
    requires: [CI]
    lock: host-a
    paths: ["**"]
    secrets_file: .kamal/secrets
    write_secrets_file: false
    secrets: [RAILS_MASTER_KEY]
    registry_password: github_token
    kamal: bundler
    ssh: {user: root, port: 22, hosts: [203.0.113.10]}
`

func TestParseRejectsUnsafeValues(t *testing.T) {
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	bad := map[string][2]string{
		"unknown key":          {"lock: host-a", "lock: host-a\n    lokc: x"},
		"shell in command":     {"command: bin/kamal", "command: bin/kamal;rm"},
		"no CI":                {"requires: [CI]", "requires: []"},
		"unknown CI":           {"requires: [CI]", "requires: [Other]"},
		"floating tool":        {"tool_version: v0.1.0", "tool_version: latest"},
		"GITHUB_ secret":       {"secrets: [RAILS_MASTER_KEY]", "secrets: [GITHUB_TOKEN]"},
		"traversal":            {"secrets_file: .kamal/secrets", "secrets_file: ../x"},
		"host option":          {"hosts: [203.0.113.10]", "hosts: [-oProxyCommand=x]"},
		"gem without version":  {"kamal: bundler", "kamal: gem"},
		"self gate":            {"name: CI", "name: kamal-autodeploy"},
		"registry secret name": {"registry_password: github_token", "registry_password: secret"},
	}
	for name, r := range bad {
		raw := strings.Replace(valid, r[0], r[1], 1)
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}

func TestOrderAndLocks(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Units[0]
	b.ID, b.Environment = "edge", "production-edge"
	c.Units = append(c.Units, b)
	cpy := c.Units[0]
	cpy.ID, cpy.Environment, cpy.After = "worker", "production-worker", []string{"edge"}
	c.Units = append(c.Units, cpy)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if p := c.Predecessors("edge"); strings.Join(p, ",") != "web" {
		t.Errorf("edge predecessors %v", p)
	}
	c.Units[0].After = []string{"worker"}
	if err := c.Validate(); err != nil {
		t.Errorf("after rules override lock order without a cycle: %v", err)
	}
	c.Units[2].After = []string{"web"}
	if err := c.Validate(); err == nil {
		t.Error("cycle must be rejected")
	}
}

func TestOutputKeyCollisionsAndReservedNames(t *testing.T) {
	c, _ := Parse([]byte(valid))
	b := c.Units[0]
	b.ID, b.Environment = "api-web", "e1"
	d := c.Units[0]
	d.ID, d.Environment = "api_web", "e2"
	c.Units = append(c.Units, b, d)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "collide") {
		t.Errorf("api-web and api_web must collide: %v", err)
	}
	for _, name := range []string{"ACTION", "DEPLOY_SHA", "ROLLBACK_VERSION", "UPSTATE_HOOK_TOKEN", "RUNNER_TEMP", "KAMAL_AUTODEPLOY_SSH_KEY"} {
		raw := strings.Replace(valid, "secrets: [RAILS_MASTER_KEY]", "secrets: ["+name+"]", 1)
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s must be reserved", name)
		}
	}
}

func TestLockChainFollowsTopologicalOrder(t *testing.T) {
	c, _ := Parse([]byte(valid))
	base := c.Units[0]
	c.Units = nil
	for _, id := range []string{"a", "b", "c"} {
		u := base
		u.ID, u.Environment = id, "env-"+id
		c.Units = append(c.Units, u)
	}
	c.Units[0].After = []string{"c"} // a after c, all on one lock
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	order, _ := c.Order()
	seen := map[string]bool{}
	for _, id := range order {
		for _, p := range c.Predecessors(id) {
			if !seen[p] {
				t.Fatalf("%s waits for %s, which comes later in %v: a cycle", id, p, order)
			}
		}
		seen[id] = true
	}
	if strings.Join(order, ",") != "b,c,a" && strings.Join(order, ",") != "c,a,b" && len(order) != 3 {
		t.Errorf("order %v", order)
	}
}

func TestEnvironmentNamesAreCaseInsensitive(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	other := cfg.Units[0]
	other.ID, other.Environment = "worker", "Production"
	cfg.Units = append(cfg.Units, other)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "share environment") {
		t.Fatalf("expected environment collision, got %v", err)
	}
}

func TestActivationCompatibilityAndValidation(t *testing.T) {
	legacy, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.Automatic() {
		t.Fatal("legacy config lost automatic behavior")
	}
	for _, mode := range []string{ActivationManual, ActivationAutomatic, "invalid"} {
		c := *legacy
		c.ToolVersion = "v0.1.4"
		c.Activation = mode
		if (c.Validate() == nil) != (mode != "invalid") {
			t.Fatalf("activation %q validation", mode)
		}
		if mode == ActivationManual && c.Automatic() {
			t.Fatal("manual activation enabled automatic work")
		}
	}
}

func TestDirectKamalRoutingIsStructured(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	u := &c.Units[0]
	u.Command = "kamal"
	u.Args = DirectArgs(u.Config, u.Destination)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-c", "config/deploy.other.yml"}, {"-c", "config/deploy.yml", "-c", "config/deploy.other.yml"}, {"-d", "other"}, {"--skip-hooks"}} {
		u.Args = args
		if c.Validate() == nil {
			t.Fatalf("conflicting direct arguments accepted: %v", args)
		}
	}
	u.Config = "../outside.yml"
	u.Args = DirectArgs(u.Config, "")
	if c.Validate() == nil {
		t.Fatal("direct config traversal accepted")
	}
}

func TestActivationRequiresCompatibleRuntimePin(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	c.Activation = ActivationManual
	if c.Validate() == nil {
		t.Fatal("new activation config accepted an incompatible old runtime")
	}
	for _, version := range []string{"v0.1.4", "v0.2.0", "v1.0.0", strings.Repeat("a", 40)} {
		c.ToolVersion = version
		if err := c.Validate(); err != nil {
			t.Fatalf("compatible pin %s: %v", version, err)
		}
	}
}

func TestTailscaleValidation(t *testing.T) {
	for _, tc := range []struct {
		name, version, tags string
		ok                  bool
	}{
		{"old runtime", "v0.1.7", "[tag:deploy]", false},
		{"release", "v0.1.8", "[tag:deploy]", true},
		{"source", strings.Repeat("a", 40), "[tag:deploy]", true},
		{"empty", "v0.1.8", "[]", false},
		{"duplicate", "v0.1.8", "[tag:deploy, tag:deploy]", false},
		{"expression", "v0.1.8", "['tag:${{ github.token }}']", false},
		{"comma injection", "v0.1.8", "['tag:deploy,tag:admin']", false},
		{"missing prefix", "v0.1.8", "[deploy]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(valid, "tool_version: v0.1.0", "tool_version: "+tc.version, 1)
			raw = strings.ReplaceAll(raw, "203.0.113.10", "100.101.102.103")
			raw += "    tailscale:\n      tags: " + tc.tags + "\n"
			_, err := Parse([]byte(raw))
			if (err == nil) != tc.ok {
				t.Fatalf("got %v, valid=%v", err, tc.ok)
			}
		})
	}
}

func TestTailscaleRejectsPublicAndLocalTargets(t *testing.T) {
	for _, host := range []string{"203.0.113.10", "127.0.0.1", "10.0.0.1", "localhost", "server.local", "100.101.102.103", "server.example.ts.net"} {
		c, err := Parse([]byte(valid))
		if err != nil {
			t.Fatal(err)
		}
		c.ToolVersion = "v0.1.8"
		c.Units[0].Tailscale = &Tailscale{Tags: []string{"tag:deploy"}}
		c.Units[0].SSH.Hosts = []string{host}
		if (c.Validate() == nil) != TailscaleHost(host) {
			t.Fatalf("unexpected target acceptance: %s", host)
		}
	}
}

func TestBatchPreviewVersionBoundary(t *testing.T) {
	for version, want := range map[string]bool{
		"v0.1.8":                                false,
		"v0.1.9-0.20261110120000-abcdefabcdef":  false,
		"v0.1.9-rc.1":                           false,
		"v0.1.9":                                true,
		"v0.1.10-0.20261111120000-abcdefabcdef": true,
		"v0.2.0":                                true,
		"bad-version":                           false,
	} {
		if got := SupportsBatchPreviewVersion(version); got != want {
			t.Errorf("%s: got %v, want %v", version, got, want)
		}
	}
}
