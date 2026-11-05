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
