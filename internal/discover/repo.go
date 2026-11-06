package discover

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
)

// Workflow is an existing GitHub Actions workflow as seen statically.
type Workflow struct {
	Path              string
	Name              string
	PushBranch        bool     // runs on push to the deploy branch
	PathFilters       []string // push paths filter (the workflow may not run for every commit)
	PullRequestTarget bool
	Unparsed          bool
}

func findWorkflows(root, branch string) ([]Workflow, []Note) {
	var out []Workflow
	var notes []Note
	matches, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	sort.Strings(matches)
	for _, m := range matches {
		rel, _ := filepath.Rel(root, m)
		rel = filepath.ToSlash(rel)
		if rel == config.WorkflowPath {
			continue
		}
		raw, err := readFile(m)
		if err != nil {
			continue
		}
		var doc map[string]any
		if yaml.Unmarshal(raw, &doc) != nil {
			out = append(out, Workflow{Path: rel, Unparsed: true})
			notes = append(notes, Note{Level: Unknown, Text: rel + " could not be parsed; it is not used as a CI gate"})
			continue
		}
		w := Workflow{Path: rel, Name: str(doc["name"])}
		if w.Name == "" {
			w.Name = rel // GitHub names an unnamed workflow by its path
		}
		// yaml.v3 (YAML 1.2) decodes the `on` key as the string "on", not a boolean.
		on := doc["on"]
		switch t := on.(type) {
		case string:
			w.PushBranch = t == "push"
			w.PullRequestTarget = t == "pull_request_target"
		case []any:
			for _, e := range t {
				if str(e) == "push" {
					w.PushBranch = true
				}
				if str(e) == "pull_request_target" {
					w.PullRequestTarget = true
				}
			}
		case map[string]any:
			if _, ok := t["pull_request_target"]; ok {
				w.PullRequestTarget = true
			}
			if push, ok := t["push"]; ok {
				p := mapping(push)
				w.PushBranch = branchMatches(p, branch)
				for _, f := range list(p["paths"]) {
					w.PathFilters = append(w.PathFilters, str(f))
				}
				if len(list(p["paths-ignore"])) > 0 {
					w.PathFilters = append(w.PathFilters, "(paths-ignore)")
				}
			}
		}
		out = append(out, w)
	}
	return out, notes
}

// branchMatches is true when a push trigger runs for branch. Filters follow GitHub's rules
// closely enough for a gate decision: `branches` patterns are evaluated in order with `!`
// negations (last match wins), `branches-ignore` excludes any match, `*` stays within a path
// segment and `**` crosses them. A push trigger limited to tags never runs for a branch.
func branchMatches(push map[string]any, branch string) bool {
	if push == nil {
		return true
	}
	patterns := func(v any) []string {
		var out []string
		for _, b := range list(v) {
			out = append(out, str(b))
		}
		return out
	}
	if push["branches-ignore"] != nil {
		for _, p := range patterns(push["branches-ignore"]) {
			if config.Match([]string{p}, branch) {
				return false
			}
		}
		return true
	}
	if push["branches"] == nil {
		return push["tags"] == nil && push["tags-ignore"] == nil
	}
	return config.Match(patterns(push["branches"]), branch)
}

// wrapperFor finds an executable bin/ script that runs Kamal with this config path
// (the example application's bin/kamal-rails appends -c config/deploy.rails.yml after the command).
func wrapperFor(root, configPath string) string {
	matches, _ := filepath.Glob(filepath.Join(root, "bin", "*"))
	sort.Strings(matches)
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 || filepath.Base(m) == "kamal" {
			continue
		}
		raw, err := readFile(m)
		if err != nil || bytes.IndexByte(raw, 0) >= 0 {
			continue
		}
		route, ok := wrapperRoute(string(raw))
		if ok && route == configPath {
			rel, _ := filepath.Rel(root, m)
			return filepath.ToSlash(rel)
		}
	}
	return ""
}

var lockPattern = regexp.MustCompile(`(?m)^    kamal \(([0-9]+\.[0-9]+\.[0-9]+)\)`)

// kamalFromGemfile returns the locked kamal version when Gemfile.lock pins it.
func kamalFromGemfile(root string) string {
	raw, err := readFile(filepath.Join(root, "Gemfile.lock"))
	if err != nil {
		return ""
	}
	if m := lockPattern.FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return ""
}

var dotenvKey = regexp.MustCompile(`^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)

// SecretLine is one entry of a Kamal secrets file. The value is never kept: only its kind.
type SecretLine struct {
	Name string
	Kind string // literal, env (exactly $NAME or ${NAME}), command ($(...)), interpolated
}

// ReadSecretNames parses a dotenv-style file for names and value kinds only.
func ReadSecretNames(path string) ([]SecretLine, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var out []SecretLine
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := dotenvKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, SecretLine{Name: m[1], Kind: valueKind(m[1], m[2])})
	}
	return out, nil
}

func valueKind(name, value string) string {
	v := strings.Trim(strings.TrimSpace(value), `"`)
	switch {
	case v == "$"+name || v == "${"+name+"}":
		return "env"
	case strings.Contains(v, "$("), strings.Contains(v, "`"):
		return "command"
	case strings.Contains(v, "$"):
		return "interpolated"
	}
	return "literal"
}

// dockerignoreNegations turns .dockerignore rules into path-ownership negations: files the
// Docker build never sees cannot change the image. Re-includes (!pattern) become positive.
func dockerignoreNegations(root, context string) []string {
	path := filepath.Join(root, context, ".dockerignore")
	raw, err := readFile(path)
	if err != nil {
		return nil
	}
	prefix := ""
	if context != "." && context != "" {
		prefix = strings.TrimSuffix(filepath.ToSlash(context), "/") + "/"
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		neg := strings.HasPrefix(line, "!")
		line = strings.TrimPrefix(line, "!")
		line = strings.Trim(strings.TrimPrefix(line, "./"), "/")
		if line == "" || strings.ContainsAny(line, "\\") {
			continue
		}
		pat := prefix + line
		rules := []string{pat, pat + "/**"}
		if strings.HasSuffix(pat, "/**") {
			rules = []string{pat}
		}
		for _, r := range rules {
			if config.ValidPattern(r) != nil {
				continue
			}
			if neg {
				out = append(out, r)
			} else {
				out = append(out, "!"+r)
			}
		}
	}
	out = append(out, prefix+".dockerignore")
	return out
}

// privateHost is true for addresses a GitHub-hosted runner normally cannot reach:
// RFC 1918, CGNAT/Tailscale (100.64.0.0/10), link-local, loopback, *.ts.net and *.local names.
func privateHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return true
		}
		_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
		return cgnat.Contains(ip)
	}
	h = strings.ToLower(h)
	return strings.HasSuffix(h, ".ts.net") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") || !strings.Contains(h, ".")
}

// UpstateMapping is the safe part of a committed Upstate mapping (.upstate/*.json).
type UpstateMapping struct {
	EnvironmentID string `json:"environment_public_id"`
	Origin        string `json:"origin"`
	Service       string `json:"service"`
	Destination   string `json:"destination"`
}

// CommandRouting accepts a deliberately bounded wrapper grammar. Comments never
// select a config, and scripts with conditional or additional executable routing fail closed.
func CommandRouting(root, command string) (string, bool) {
	if command == "kamal" {
		return "", true
	}
	raw, err := readFile(filepath.Join(root, command))
	if err != nil {
		return "", false
	}
	// The standard binstub may load a sibling bin/bundle. Do not approve an
	// executable we have not inspected, even if its parent binstub is standard.
	if isStandardBundlerBinstub(string(raw)) {
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, command))
		if err != nil {
			return "", false
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", false
		}
		// Ruby resolves __dir__ through parent symlinks. The standard body must
		// still select the repository-root Gemfile, not another directory's file.
		if filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "Gemfile")) != filepath.Join(resolvedRoot, "Gemfile") {
			return "", false
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(resolved), "bundle")); !os.IsNotExist(err) {
			return "", false
		}
	}
	body := strings.Join(wrapperLines(string(raw)), "\n")
	ruby := isStandardBundlerBinstub(string(raw)) || body == "require \"rubygems\"\nrequire \"bundler/setup\"\nload Gem.bin_path(\"kamal\", \"kamal\")"
	header := strings.SplitN(string(raw), "\n", 2)[0]
	if ruby {
		if !plainRubyShebang(header) {
			return "", false
		}
	} else if header != "#!/bin/sh" && header != "#!/bin/bash" && header != "#!/usr/bin/env sh" && header != "#!/usr/bin/env bash" {
		return "", false
	}
	return wrapperRoute(string(raw))
}

var wrapperExec = regexp.MustCompile(`^exec (?:bundle exec )?kamal "\$@"(?: -c (?:"\$root/([A-Za-z0-9_./-]+)"|([A-Za-z0-9_./-]+)))?$`)

func wrapperLines(raw string) []string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

// Exact executable body emitted by Bundler for bin/kamal. Additional routing,
// argument mutation or a different Gemfile is deliberately not accepted.
const standardBundlerBinstub = "ENV[\"BUNDLE_GEMFILE\"] ||= File.expand_path(\"../Gemfile\", __dir__)\n" +
	"\n" +
	"bundle_binstub = File.expand_path(\"bundle\", __dir__)\n" +
	"\n" +
	"if File.file?(bundle_binstub)\n" +
	"  if File.read(bundle_binstub, 300).include?(\"This file was generated by Bundler\")\n" +
	"    load(bundle_binstub)\n" +
	"  else\n" +
	"    abort(\"Your `bin/bundle` was not generated by Bundler, so this binstub cannot run.\n" +
	"Replace `bin/bundle` by running `bundle binstubs bundler --force`, then run this command again.\")\n" +
	"  end\n" +
	"end\n" +
	"\n" +
	"require \"rubygems\"\n" +
	"require \"bundler/setup\"\n" +
	"\n" +
	"load Gem.bin_path(\"kamal\", \"kamal\")"

// Require a plain Ruby interpreter before ignoring header comments. Preserve the body
// byte-for-byte, including multiline string content and internal indentation.
func plainRubyShebang(header string) bool {
	return header == "#!/usr/bin/env ruby" || header == "#!/usr/bin/ruby"
}

func isStandardBundlerBinstub(raw string) bool {
	lines := strings.Split(raw, "\n")
	// The workflow executes the file directly: a matching Ruby body cannot
	// approve a different interpreter or options that load additional code.
	if len(lines) == 0 || !plainRubyShebang(lines[0]) {
		return false
	}
	lines = lines[1:]
	for len(lines) > 0 && (strings.TrimSpace(lines[0]) == "" || strings.HasPrefix(strings.TrimSpace(lines[0]), "#")) {
		lines = lines[1:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") == standardBundlerBinstub
}

func wrapperRoute(raw string) (string, bool) {
	lines := wrapperLines(raw)
	if isStandardBundlerBinstub(raw) {
		return "", true
	}
	if strings.Join(lines, "\n") == "require \"rubygems\"\nrequire \"bundler/setup\"\nload Gem.bin_path(\"kamal\", \"kamal\")" {
		return "", true
	}
	route, found := "", false
	rootSet := false
	for _, line := range lines {
		if line == "set -euo pipefail" || line == "set -eu" || line == "set -e" {
			continue
		}
		if line == `root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"` {
			if rootSet {
				return "", false
			}
			rootSet = true
			continue
		}
		match := wrapperExec.FindStringSubmatch(line)
		if match == nil || found {
			return "", false
		}
		if match[1] != "" && !rootSet {
			return "", false
		}
		found = true
		route = match[1]
		if route == "" {
			route = match[2]
		}
	}
	return route, found
}
