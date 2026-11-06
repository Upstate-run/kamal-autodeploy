package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/discover"
	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
)

// ready is an assessment, not a prerequisite that mutates the repository. It uses
// the same discovery and GitHub protection checks as init/doctor/provision.
func readyCommand(e *Env, args []string) error {
	f := newFlags("ready")
	offline := f.Bool("offline", false, "")
	direct := f.Bool("direct-kamal", false, "")
	branch := f.String("branch", "main", "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return errors.New("usage: kamal-autodeploy ready [--offline] [--direct-kamal] [--branch main]")
	}
	root, err := repoRoot(e.Dir)
	if err != nil {
		return err
	}
	owner, name := repoSlug(root)
	var cfg *config.Config
	reviewed := false
	if raw, err := os.ReadFile(filepath.Join(root, config.FileName)); err == nil {
		cfg, err = config.Parse(raw)
		if err != nil {
			return errors.New("reviewed config is invalid; correct it before assessing readiness")
		}
		reviewed = true
		override := false
		f.Visit(func(flag *flag.Flag) {
			if flag.Name == "direct-kamal" || flag.Name == "branch" {
				override = true
			}
		})
		if override {
			return errors.New("ready uses the reviewed config; --direct-kamal and --branch apply only before init")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	discoveryBranch := *branch
	if reviewed {
		discoveryBranch = cfg.Branch
	}
	report, err := discover.Discover(root, discover.Options{Branch: discoveryBranch, Owner: owner, ToolVersion: ToolVersion(), DirectKamal: *direct})
	if err != nil {
		return err
	}
	if !reviewed {
		cfg = report.Config
	}
	var checks []check
	add := func(level, format string, a ...any) { checks = append(checks, check{level, fmt.Sprintf(format, a...)}) }
	e.heading("REPOSITORY READINESS")
	fmt.Fprintln(e.Out, "Read-only assessment. Project scripts, ERB, secret commands and SSH are never executed.")
	if reviewed {
		add("ok", "using reviewed %s (activation %s)", config.FileName, cfg.EffectiveActivation())
	} else {
		add("action", "no reviewed config yet; init will prepare a manual workflow")
		for _, n := range report.Notes {
			level := "warn"
			switch n.Level {
			case discover.Info:
				continue
			case discover.Unsupported:
				level = "fail"
			case discover.Unknown:
				level = "unknown"
			}
			prefix := ""
			if n.Unit != "" {
				prefix = n.Unit + ": "
			}
			add(level, "%s%s", prefix, n.Text)
		}
	}
	if !reviewed {
		proposal := *cfg
		// Validate repository shape even in a development build with no install pin.
		// This placeholder is used only in the copy; never persisted or recommended.
		if proposal.ToolVersion == "" {
			proposal.ToolVersion = strings.Repeat("0", 40)
			add("unknown", "development build has no install pin; init requires an explicit --tool-version")
		}
		if err := proposal.Validate(); err != nil {
			add("fail", "proposed config is not valid yet: %v", err)
		}
	}
	if len(cfg.CI) == 0 {
		add("fail", "no CI gate configured; add a workflow that runs on every push to the deploy branch")
	}
	ciNames := map[string]bool{}
	unconditional := false
	for _, w := range cfg.CI {
		if ciNames[w.Name] {
			add("fail", "CI workflow %q is listed twice; give each gate a unique name", w.Name)
		}
		ciNames[w.Name] = true
		matched := false
		for _, discovered := range report.Workflows {
			if discovered.Path != w.Path || discovered.Name != w.Name || discovered.Unparsed || !discovered.PushBranch {
				continue
			}
			matched = true
			if len(discovered.PathFilters) == 0 {
				unconditional = true
			} else {
				add("warn", "CI gate %s is path-filtered and may not run for every commit", w.Name)
			}
		}
		if !matched {
			add("fail", "CI gate %s (%s) is missing, renamed or not statically known to run on push to %s", w.Name, w.Path, cfg.Branch)
		} else {
			add("ok", "CI gate %s (%s)", w.Name, w.Path)
		}
	}
	if len(cfg.CI) > 0 && !unconditional {
		add("fail", "at least one configured CI gate must run on every push to %s", cfg.Branch)
	}
	for _, u := range cfg.Units {
		add("info", "%s → environment %s\n        config: %s\n        runtime: %s\n        SSH: %s@%s (port %d)\n        required app secret names: %s", u.ID, u.Environment, u.Config, kamalLabel(u), u.SSH.User, strings.Join(u.SSH.Hosts, ", "), u.SSH.Port, orNone(strings.Join(u.Secrets, ", ")))
		if u.Command == "kamal" {
			if !config.MatchDirectArgs(u.Config, u.Destination, u.Args) {
				add("fail", "%s: direct command arguments do not match config/destination", u.ID)
			}
		} else if route, ok := discover.CommandRouting(root, u.Command); !ok || (route != "" && route != u.Config) {
			add("fail", "%s: wrapper routing is unsupported; review init --direct-kamal to leave the laptop wrapper unchanged", u.ID)
		}
		// Compare the selected current Kamal tree with the reviewed plan, using the
		// same selection identity and wrapper routing rules as verify-hosts.
		for _, current := range report.Config.Units {
			if current.Config != u.Config || current.Destination != u.Destination {
				continue
			}
			if len(current.SSH.Hosts) == 0 {
				add("fail", "%s: selected Kamal config has no SSH targets", u.ID)
			}
			if u.Command != "kamal" && (u.Command != current.Command || !slices.Equal(u.Args, current.Args)) {
				add("fail", "%s: wrapper command/arguments differ from discovered config/destination routing; explicitly reconcile the reviewed plan", u.ID)
			}

			for _, name := range current.Secrets {
				if name == "KAMAL_REGISTRY_PASSWORD" && u.RegistryPassword == config.RegistryGitHubToken {
					continue
				}
				if !slices.Contains(u.Secrets, name) {
					add("fail", "%s: newly discovered secret %s is not exported by the reviewed plan; explicitly reconcile required secrets", u.ID, name)
				}
			}
			covered := current.SSH.Port == u.SSH.Port
			for _, host := range current.SSH.Hosts {
				covered = covered && slices.Contains(u.SSH.Hosts, host)
			}
			if !covered {
				add("fail", "%s: current Kamal hosts/port are not covered by the reviewed SSH settings; explicitly reconcile the reviewed plan and host pins", u.ID)
			}
		}
		found := false
		for _, info := range report.Units {
			if info.Base != u.Config || info.Destination != u.Destination {
				continue
			}
			found = true
			if info.SSHUserUnknown {
				add("unknown", "%s: Kamal SSH user is unresolved; compatibility with the reviewed login remains unverified", u.ID)
			} else {
				for _, current := range report.Config.Units {
					if current.Config == u.Config && current.Destination == u.Destination && current.SSH.User != u.SSH.User {
						add("fail", "%s: current Kamal SSH user differs from the reviewed SSH user; explicitly reconcile the reviewed plan and CI key authorization", u.ID)
					}
				}
			}
			if u.RegistryPassword == config.RegistryGitHubToken {
				if info.RegistryServerUnknown {
					add("unknown", "%s: registry server is unresolved; compatibility with the reviewed GitHub-token policy remains unverified", u.ID)
				} else if info.RegistryServer != "ghcr.io" {
					add("fail", "%s: current registry server is incompatible with the reviewed GitHub-token policy; explicitly reconcile registry authentication", u.ID)
				}
			}
			if info.SecretsPathUnknown {
				add("unknown", "%s: Kamal secrets_path is unresolved; actual secrets file path remains unverified", u.ID)
			} else if info.SecretsFile != u.SecretsFile {
				add("fail", "%s: current Kamal secrets_path differs from reviewed secrets_file; explicitly reconcile the reviewed plan", u.ID)
			}
			if reviewed {
				for _, issue := range info.SecretIssues {
					level := "unknown"
					if issue.Level == discover.Unsupported {
						level = "fail"
					}
					add(level, "%s: %s", u.ID, issue.Text)
				}
			}
			if len(info.PrivateHosts) > 0 {
				add("fail", "%s: private SSH targets need runner networking that the generated workflow does not configure", u.ID)
			}
			if info.Dynamic {
				add("unknown", "%s: Kamal config contains ERB; review runtime environment and local-file dependencies. ERB is not evaluated", u.ID)
			}
			if info.HostsUnknown || info.EndpointUnknown {
				add("fail", "%s: SSH routing is unresolved; use explicit hosts, user and port", u.ID)
			}
		}
		if !found {
			add("fail", "%s: selected config/destination is not statically discoverable", u.ID)
		}
	}
	// Kamal may load secrets-common in addition to the selected secrets file.
	// Parse names/kinds only, and only tracked files that a fresh runner receives.
	g := gitx.Git{Dir: root}
	secretFiles := map[string]bool{".kamal/secrets-common": true}
	for _, u := range cfg.Units {
		secretFileChecks(root, u, add)
		secretFiles[u.SecretsFile] = true
	}
	for _, path := range sortedSecretFiles(secretFiles) {
		if !g.Tracked(path) {
			continue
		}
		lines, err := discover.ReadSecretNames(filepath.Join(root, path))
		if err != nil {
			add("fail", "%s: cannot inspect tracked secrets file safely", path)
			continue
		}
		for _, line := range lines {
			if line.Kind == "command" || line.Kind == "interpolated" {
				add("warn", "%s: tracked secrets entry %s depends on a command or other variables; review runner availability and prefer CI environment lookup (value not shown)", path, line.Name)
			}
		}
	}

	hooks, _ := filepath.Glob(filepath.Join(root, ".kamal", "hooks", "*"))
	for _, path := range hooks {
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			continue
		}
		rel, _ := filepath.Rel(root, path)
		add("unknown", "hook %s: review executable permissions, local file/command dependencies and composed hooks; custom code is not executed", filepath.ToSlash(rel))
	}
	failed := e.checks(checks)
	e.heading("GITHUB SETUP")
	checks = nil
	ghOK := false
	if *offline {
		add("unknown", "offline: GitHub environments, secret presence, host pins and package access were not checked")
	} else {
		ghOK = githubChecks(e, cfg, root, add)
	}
	setupFailed := e.checks(checks)
	e.heading("REGISTRY ACCESS")
	registryChecks(e, cfg, report, owner, name, ghOK)
	e.heading("RUNTIME VERIFICATION")
	fmt.Fprintln(e.Out, "unknown Runner SSH authorization, deployment health, receipts and notifications require explicit remote doctor and a manual deployment; they are not established by this assessment.")
	if failed {
		e.next("Fix the failed checks", "Rerun kamal-autodeploy ready after addressing the failures. No files were changed.")
		return errors.New("readiness found problems (fail lines above)")
	}
	if reviewed && setupFailed {
		e.next("Complete GitHub setup", "Run kamal-autodeploy provision to review the missing setup, then rerun doctor. Existing protections are never relaxed automatically.")
		return errors.New("GitHub setup is incomplete (fail lines above)")
	}
	if reviewed {
		e.next("Check the published setup", "Run kamal-autodeploy doctor for workflow consistency and setup details. Resolve warnings and unknowns before a manual deployment.")
	} else {
		command := "kamal-autodeploy init"
		if *direct {
			command += " --direct-kamal"
		}
		if *branch != "main" {
			command += " --branch " + shellQuote(*branch)
		}
		e.next("Review and prepare the manual workflow", "Resolve the warnings and unknowns above, then run:\n  "+command+"\nReview the generated files before committing. No files were changed by ready.")
	}
	if setupFailed {
		return errors.New("GitHub setup is incomplete; prepare the reviewed config before provisioning")
	}
	return nil
}

func registryChecks(e *Env, cfg *config.Config, r *discover.Report, owner, repo string, online bool) {
	for _, u := range cfg.Units {
		if u.RegistryPassword != config.RegistryGitHubToken {
			continue
		}
		image := ""
		for _, info := range r.Units {
			if info.Base == u.Config && info.Destination == u.Destination {
				image = info.Image
			}
		}
		parts := strings.SplitN(image, "/", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], owner) || parts[1] == "" {
			e.checks([]check{{"unknown", u.ID + ": GHCR package could not be identified statically; verify this repository's Actions write access in package settings"}})
			continue
		}
		packageName := parts[1]
		accountPath := ""
		if online {
			out, err := e.Runner.Run(nil, "gh", "api", "users/"+owner)
			var account struct {
				Type string `json:"type"`
			}
			if err == nil && json.Unmarshal(out, &account) == nil {
				switch account.Type {
				case "Organization":
					accountPath = "orgs"
				case "User":
					accountPath = "users"
				}
			}
			if accountPath != "" {
				out, err = e.Runner.Run(nil, "gh", "api", accountPath+"/"+owner+"/packages/container/"+url.PathEscape(packageName))
				var metadata struct {
					Repository *struct {
						FullName string `json:"full_name"`
					} `json:"repository"`
				}
				if err == nil && json.Unmarshal(out, &metadata) == nil {
					if metadata.Repository != nil && strings.EqualFold(metadata.Repository.FullName, owner+"/"+repo) {
						e.checks([]check{{"ok", u.ID + ": package is linked to this repository (Actions write access is not proven by the link)"}})
					}
				} else {
					e.checks([]check{{"unknown", u.ID + ": package metadata unavailable (permissions or package absence); this does not prove Actions access is missing"}})
				}
			}
		}
		settings := "https://github.com/" + accountPath + "/" + owner + "/packages/container/" + url.PathEscape(packageName) + "/settings"
		if accountPath == "" {
			settings = "https://github.com/" + owner + "?tab=packages (open the " + packageName + " container package, then Package settings)"
		}
		e.checks([]check{{"unknown", u.ID + ": verify existing Actions write access at " + settings}})
		fmt.Fprintln(e.Out, "        Package settings → Manage Actions access: this repository needs Write or Admin. Existing inherited access may already suffice; add access only if missing. No PAT is required by the generated job.")
	}
}

func sortedSecretFiles(files map[string]bool) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
