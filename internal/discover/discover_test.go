package discover

import (
	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	for p, c := range files {
		path := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const ci = "name: CI\non:\n  push:\n    branches: [main]\njobs: {}\n"

func notes(r *Report, level string) string {
	var out []string
	for _, n := range r.Notes {
		if n.Level == level {
			out = append(out, n.Unit+": "+n.Text)
		}
	}
	return strings.Join(out, "\n")
}

func TestDestinationOverlaysBecomeUnits(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		".github/workflows/ci.yml":     ci,
		"config/deploy.yml":            "service: shop\nimage: acme/shop\nminimum_version: 2.12.0\nregistry:\n  server: ghcr.io\n  username: acme\n  password: [KAMAL_REGISTRY_PASSWORD]\n",
		"config/deploy.production.yml": "servers:\n  web: [203.0.113.10]\n",
		"config/deploy.staging.yml":    "servers:\n  web: [203.0.113.20]\n",
	})
	r, err := Discover(root, Options{Owner: "acme", ToolVersion: "v0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Config.Units) != 2 {
		t.Fatalf("units %+v", r.Config.Units)
	}
	p := r.Config.Units[0]
	if p.ID != "production" || p.Environment != "production" || strings.Join(p.Args, " ") != "-d production" || p.SecretsFile != ".kamal/secrets.production" {
		t.Errorf("production unit %+v", p)
	}
	if r.Config.Units[0].Lock == r.Config.Units[1].Lock {
		t.Error("different hosts must not share a lock")
	}
	if err := r.Config.Validate(); err != nil {
		t.Error(err)
	}
}

func TestERBAndPrivateHostsAreUnknownNotGuessed(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		".github/workflows/ci.yml": ci,
		"config/deploy.yml":        "service: shop\nimage: acme/shop\nminimum_version: 2.12.0\nservers:\n  web:\n    - <%= ENV['HOST'] %>\n    - 100.101.102.103\nssh:\n  proxy: bastion.example.com\n",
	})
	r, err := Discover(root, Options{ToolVersion: "v0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	u := r.Config.Units[0]
	if strings.Join(u.SSH.Hosts, ",") != "100.101.102.103" {
		t.Errorf("hosts %v: a templated host must not be guessed", u.SSH.Hosts)
	}
	unsupported := notes(r, Unsupported)
	if !strings.Contains(unsupported, "templated") {
		t.Error("a templated host would go unpinned: it must block init")
	}
	if !strings.Contains(unsupported, "Tailscale") || !strings.Contains(unsupported, "ssh.proxy") {
		t.Errorf("private host and jump host must be unsupported:\n%s", unsupported)
	}
}

func TestNoCIIsUnsupported(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"config/deploy.yml": "service: shop\nservers: [203.0.113.10]\n", ".github/workflows/pr.yml": "name: PR\non: pull_request\njobs: {}\n"})
	r, err := Discover(root, Options{ToolVersion: "v0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Blocking()) == 0 {
		t.Error("no push CI must block")
	}
}

func TestBranchFilters(t *testing.T) {
	cases := map[string]bool{
		"name: A\non:\n  push:\n    branches: ['**', '!main']\njobs: {}\n":        false,
		"name: A\non:\n  push:\n    branches-ignore: ['m*']\njobs: {}\n":          false,
		"name: A\non:\n  push:\n    branches: ['ma*']\njobs: {}\n":                true,
		"name: A\non:\n  push:\n    tags: ['v*']\njobs: {}\n":                     false,
		"name: A\non: [push, pull_request]\njobs: {}\n":                           true,
		"name: A\non:\n  push:\n    branches: ['release/**', 'main']\njobs: {}\n": true,
	}
	for wf, want := range cases {
		root := t.TempDir()
		write(t, root, map[string]string{".github/workflows/a.yml": wf})
		got, _ := findWorkflows(root, "main")
		if got[0].PushBranch != want {
			t.Errorf("%q: runs on main = %v, want %v", wf, got[0].PushBranch, want)
		}
	}
}

func TestWrapperRoutingIgnoresCommentsAndRejectsAmbiguity(t *testing.T) {
	route, ok := wrapperRoute("# config/deploy.rails.yml\nexec kamal \"$@\" -c config/deploy.edge.yml\n")
	if !ok || route != "config/deploy.edge.yml" {
		t.Fatalf("route %q, %v", route, ok)
	}
	for _, raw := range []string{"# kamal config/deploy.rails.yml\necho hello", "exec kamal \"$@\" -c config/deploy.rails.yml\nexec kamal \"$@\" -c config/deploy.edge.yml"} {
		if _, ok := wrapperRoute(raw); ok {
			t.Fatal("ambiguous wrapper accepted")
		}
	}
}

func TestDockerignoreAlwaysOwnsItself(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"services/edge/.dockerignore": ".dockerignore\n"})
	rules := append([]string{"services/edge/**"}, dockerignoreNegations(dir, "services/edge")...)
	if !config.Match(rules, "services/edge/.dockerignore") {
		t.Fatal("ignore file lost ownership")
	}
}

const testRubyBinstub = "#!/usr/bin/env ruby\n" + standardBundlerBinstub

func TestStandardBundlerBinstubRouting(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"bin/kamal": "#!/usr/bin/env ruby\n# This file was generated by Bundler.\n" + standardBundlerBinstub})
	if route, ok := CommandRouting(root, "bin/kamal"); !ok || route != "" {
		t.Fatalf("standard binstub rejected: %q %v", route, ok)
	}
	for _, raw := range []string{
		testRubyBinstub + "\nARGV << '-c'\nARGV << 'config/deploy.other.yml'\n",
		strings.Replace(testRubyBinstub, "../Gemfile", "../OtherGemfile", 1),
		strings.Replace(testRubyBinstub, `load Gem.bin_path("kamal", "kamal")`, `load Gem.bin_path("other", "other")`, 1),
	} {
		write(t, root, map[string]string{"bin/kamal": raw})
		if _, ok := CommandRouting(root, "bin/kamal"); ok {
			t.Fatal("modified executable binstub accepted")
		}
	}
	write(t, root, map[string]string{"bin/kamal": testRubyBinstub, "bin/bundle": "# This file was generated by Bundler.\nARGV << '-c'\n"})
	if _, ok := CommandRouting(root, "bin/kamal"); ok {
		t.Fatal("uninspected sibling bundle loader accepted")
	}
}

func TestBundlerBinstubRejectsMultilineStringInjection(t *testing.T) {
	injection := `#"); end; end; ARGV.concat(["-c", "config/deploy.other.yml"]); if false; if false; abort("`
	raw := strings.Replace(testRubyBinstub, "Replace ", injection+"\nReplace ", 1)
	if isStandardBundlerBinstub(raw) {
		t.Fatal("strict matcher accepted string injection")
	}
	if _, ok := wrapperRoute(raw); ok {
		t.Fatal("routing accepted string injection")
	}
	root := t.TempDir()
	write(t, root, map[string]string{"bin/kamal": raw})
	if _, ok := CommandRouting(root, "bin/kamal"); ok {
		t.Fatal("command accepted string injection")
	}
}

func TestBundlerBinstubRejectsSymlinkedParentRouting(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"tools/bin/kamal": testRubyBinstub, "tools/Gemfile": "source 'https://rubygems.org'", "Gemfile": "source 'https://rubygems.org'"})
	if err := os.Symlink("tools/bin", filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	if _, ok := CommandRouting(root, "bin/kamal"); ok {
		t.Fatal("symlinked parent changed Gemfile routing")
	}
}

func TestBundlerBinstubAllowsSymlinkedRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"bin/kamal": testRubyBinstub})
	alias := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, ok := CommandRouting(alias, "bin/kamal"); !ok {
		t.Fatal("repository root alias rejected")
	}
}

func TestBundlerBinstubRejectsAlternateInterpreter(t *testing.T) {
	root := t.TempDir()
	for _, header := range []string{
		"#!/bin/sh /path/to/loader",
		"#!/usr/bin/env -S ruby -r/path/to/loader",
		"#!/usr/bin/ruby -r/path/to/loader",
		"#!/usr/bin/env ruby -r/path/to/loader",
		"", // An executable standard binstub must explicitly select Ruby.
	} {
		raw := header + "\n" + standardBundlerBinstub
		write(t, root, map[string]string{"bin/kamal": raw})
		if isStandardBundlerBinstub(raw) {
			t.Fatalf("matcher accepted interpreter %q", header)
		}
		if _, ok := wrapperRoute(raw); ok {
			t.Fatalf("routing accepted interpreter %q", header)
		}
		if _, ok := CommandRouting(root, "bin/kamal"); ok {
			t.Fatalf("command accepted interpreter %q", header)
		}
	}
	for _, header := range []string{"#!/usr/bin/env ruby", "#!/usr/bin/ruby"} {
		raw := header + "\n# Bundler header\n\n" + standardBundlerBinstub
		write(t, root, map[string]string{"bin/kamal": raw})
		if _, ok := CommandRouting(root, "bin/kamal"); !ok {
			t.Fatalf("supported interpreter rejected: %q", header)
		}
	}
}

func TestCommandRoutingChecksLegacyWrapperInterpreters(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		body, header string
		want         bool
	}{
		{"exec kamal \"$@\"", "#!/bin/sh", true},
		{"exec bundle exec kamal \"$@\"", "#!/usr/bin/env bash", true},
		{"exec kamal \"$@\"", "#!/bin/sh /path/to/loader", false},
		{"exec kamal \"$@\"", "#!/usr/bin/env ruby", false},
		{"require \"rubygems\"\nrequire \"bundler/setup\"\nload Gem.bin_path(\"kamal\", \"kamal\")", "#!/bin/sh /path/to/loader", false},
	}
	for _, c := range cases {
		write(t, root, map[string]string{"bin/kamal": c.header + "\n" + c.body})
		if _, ok := CommandRouting(root, "bin/kamal"); ok != c.want {
			t.Fatalf("interpreter %q: got %v want %v", c.header, ok, c.want)
		}
	}
}

func TestExecutableWrappersRejectCRLFShebangs(t *testing.T) {
	root := t.TempDir()
	for _, raw := range []string{
		strings.ReplaceAll(testRubyBinstub, "\n", "\r\n"),
		strings.Replace(testRubyBinstub, "\n", "\r\n", 1),
		"#!/bin/sh\r\nexec kamal \"$@\"\n",
		"#!/usr/bin/env ruby\r\nrequire \"rubygems\"\nrequire \"bundler/setup\"\nload Gem.bin_path(\"kamal\", \"kamal\")",
	} {
		write(t, root, map[string]string{"bin/kamal": raw})
		if isStandardBundlerBinstub(raw) {
			t.Fatal("standard matcher normalized an unusable CRLF shebang")
		}
		if _, ok := CommandRouting(root, "bin/kamal"); ok {
			t.Fatal("command verification accepted CRLF shebang")
		}
	}
}
