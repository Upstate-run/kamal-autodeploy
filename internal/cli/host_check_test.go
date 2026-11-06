package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectedCheckoutHostsMustBePinned(t *testing.T) {
	for _, target := range []string{"198.51.100.20", "198.51.100.21", "<%= ENV.fetch('HOST') %>"} {
		t.Run(target, func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			path := filepath.Join(dir, "config/deploy.yml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), "198.51.100.20", target)), 0600); err != nil {
				t.Fatal(err)
			}
			e, _ := testEnv(dir)
			e.Getenv = func(key string) string {
				if key == "KNOWN_HOSTS" {
					return "198.51.100.20 ssh-ed25519 AAAAHOSTKEY"
				}
				return ""
			}
			err = Run(e, []string{"verify-hosts", "--config", "config/deploy.yml", "--command", "bin/kamal"})
			if (err == nil) != (target == "198.51.100.20") {
				t.Fatalf("host check: %v", err)
			}
		})
	}
}

func TestSelectedCheckoutRefusesUnresolvedPort(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	path := filepath.Join(dir, "config/deploy.yml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\nssh:\n  port: <%= ENV.fetch('SSH_PORT', 2222) %>\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	e, _ := testEnv(dir)
	e.Getenv = func(string) string { return "198.51.100.20 ssh-ed25519 AAAAHOSTKEY" }
	if err := Run(e, []string{"verify-hosts", "--config", "config/deploy.yml", "--command", "bin/kamal"}); err == nil {
		t.Fatal("unresolved port accepted")
	}
}

func TestSelectedCheckoutChecksEffectiveArgsAndWrapperRouting(t *testing.T) {
	for _, mode := range []string{"wrong args", "changed exec"} {
		t.Run(mode, func(t *testing.T) {
			dir := fixtureRepo(t, "two-services", "https://github.com/acme/demo.git")
			command := "kamal"
			args := []string{"-c", "config/deploy.edge.yml"}
			if mode == "wrong args" {
				mustRemove := []string{"bin/kamal-rails", "bin/kamal-edge"}
				for _, p := range mustRemove {
					if err := os.Remove(filepath.Join(dir, p)); err != nil {
						t.Fatal(err)
					}
				}
				args = []string{"-c", "config/deploy.rails.yml"}
			} else {
				command = "bin/kamal-edge"
				args = nil
				path := filepath.Join(dir, command)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// Preserve comments; change only executable routing.
				raw = []byte(strings.Replace(string(raw), `exec kamal "$@" -c "$root/config/deploy.edge.yml"`, `exec kamal "$@" -c "$root/config/deploy.rails.yml"`, 1))
				if err := os.WriteFile(path, raw, 0755); err != nil {
					t.Fatal(err)
				}
			}
			e, _ := testEnv(dir)
			e.Getenv = func(string) string { return "203.0.113.10 ssh-ed25519 AAAAHOSTKEY" }
			flags := []string{"verify-hosts", "--config", "config/deploy.edge.yml", "--command", command, "--"}
			flags = append(flags, args...)
			if err := Run(e, flags); err == nil {
				t.Fatal("conflicting routing accepted")
			}
		})
	}
}

func TestDirectKamalIgnoresUnusedWrapperButVerifiesRouting(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	// A normal laptop wrapper is intentionally not run or interpreted by direct CI.
	path := filepath.Join(dir, "bin/kamal")
	raw := []byte("#!/bin/sh\nset -eu\ncd \"$(dirname \"$0\")/..\"\nexec kamal \"$@\" -c config/deploy.yml\n")
	if err := os.WriteFile(path, raw, 0755); err != nil {
		t.Fatal(err)
	}
	e, _ := testEnv(dir)
	e.Getenv = func(string) string { return "198.51.100.20 ssh-ed25519 AAAAHOSTKEY" }
	flags := []string{"verify-hosts", "--config", "config/deploy.yml", "--command", "kamal", "--", "-c", "config/deploy.yml"}
	if err := Run(e, flags); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("direct mode modified the wrapper")
	}
	flags[len(flags)-1] = "config/deploy.other.yml"
	if Run(e, flags) == nil {
		t.Fatal("direct config mismatch accepted")
	}
	e.Getenv = func(string) string { return "198.51.100.21 ssh-ed25519 AAAAHOSTKEY" }
	flags[len(flags)-1] = "config/deploy.yml"
	if Run(e, flags) == nil {
		t.Fatal("direct mode approved an unpinned target")
	}
}

func TestDirectKamalSelectsStandaloneConfigWithWrapperPresent(t *testing.T) {
	dir := fixtureRepo(t, "two-services", "git@github.com:acme/shop.git")
	e, _ := testEnv(dir)
	e.Getenv = func(string) string { return "203.0.113.10 ssh-ed25519 AAAAHOSTKEY" }
	for _, args := range [][]string{{"-c", "config/deploy.edge.yml"}, {"-c", "config/deploy.rails.yml"}} {
		flags := append([]string{"verify-hosts", "--config", "config/deploy.edge.yml", "--command", "kamal", "--"}, args...)
		err := Run(e, flags)
		if (err == nil) != (args[1] == "config/deploy.edge.yml") {
			t.Fatalf("routing %v: %v", args, err)
		}
	}
}

func TestDirectKamalChecksDestinationOverlayHosts(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	if err := os.WriteFile(filepath.Join(dir, "config/deploy.staging.yml"), []byte("servers:\n  web: [198.51.100.21]\n  job:\n    hosts: [198.51.100.21]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e, _ := testEnv(dir)
	args := []string{"verify-hosts", "--config", "config/deploy.yml", "--destination", "staging", "--command", "kamal", "--", "-c", "config/deploy.yml", "-d", "staging"}
	e.Getenv = func(string) string { return "198.51.100.20 ssh-ed25519 AAAAHOSTKEY" }
	if Run(e, args) == nil {
		t.Fatal("base host pin approved a different destination host")
	}
	e.Getenv = func(string) string { return "198.51.100.21 ssh-ed25519 AAAAHOSTKEY" }
	if err := Run(e, args); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = "other"
	if Run(e, args) == nil {
		t.Fatal("destination argument mismatch accepted")
	}
}
