package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/generate"
	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
)

type check struct {
	level string // ok, warn, fail
	text  string
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var kamalDeployInWorkflow = regexp.MustCompile(`(?m)\bkamal(-[a-z0-9_-]+)?\s+(deploy|redeploy|rollback)\b`)

func doctorCommand(e *Env, args []string) error {
	f := newFlags("doctor")
	remote := f.Bool("remote", false, "")
	unitFlag := f.String("unit", "", "")
	yes := f.Bool("yes", false, "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return errors.New("usage: kamal-autodeploy doctor [--remote --unit ID [--yes]]")
	}
	cfg, root, err := loadRepoConfig(e)
	if err != nil {
		return err
	}
	var checks []check
	add := func(level, format string, a ...any) { checks = append(checks, check{level, fmt.Sprintf(format, a...)}) }
	add("ok", "activation is %s", cfg.EffectiveActivation())
	g := gitx.Git{Dir: root}

	// Local files.
	wf, err := generate.Workflow(cfg)
	if err != nil {
		add("fail", "config: %v", err)
	} else {
		existing, _ := os.ReadFile(filepath.Join(root, config.WorkflowPath))
		switch generate.Classify(existing, wf) {
		case generate.Current:
			add("ok", "%s matches %s", config.WorkflowPath, config.FileName)
		case generate.Absent:
			add("fail", "%s is missing; run kamal-autodeploy init", config.WorkflowPath)
		case generate.Outdated:
			add("fail", "%s is out of date; run kamal-autodeploy init", config.WorkflowPath)
		default:
			add("warn", "%s was edited by hand; keep it in sync with %s yourself", config.WorkflowPath, config.FileName)
		}
	}
	for _, p := range []string{config.FileName, config.WorkflowPath} {
		if !g.Tracked(p) {
			add("warn", "%s is not committed yet", p)
		}
	}
	for _, w := range cfg.CI {
		if _, err := os.Stat(filepath.Join(root, w.Path)); err != nil {
			add("fail", "CI workflow %q (%s) does not exist", w.Name, w.Path)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	for _, m := range matches {
		rel, _ := filepath.Rel(root, m)
		if filepath.ToSlash(rel) == config.WorkflowPath {
			continue
		}
		if raw, err := os.ReadFile(m); err == nil && kamalDeployInWorkflow.Match(raw) {
			add("warn", "%s also runs a Kamal deploy; disable it so two workflows never deploy at once", filepath.ToSlash(rel))
		}
	}
	upstateHooks := false
	if raw, err := os.ReadFile(filepath.Join(root, ".kamal", "hooks", "pre-deploy")); err == nil && strings.Contains(string(raw), "Upstate managed hook") {
		upstateHooks = true
	}
	for _, u := range cfg.Units {
		if strings.Contains(u.Command, "/") {
			info, err := os.Stat(filepath.Join(root, u.Command))
			if err != nil || info.Mode()&0o111 == 0 {
				add("fail", "%s: command %s is not an executable file", u.ID, u.Command)
			}
		}
		tracked := g.Tracked(u.SecretsFile)
		switch {
		case u.WriteSecretsFile && tracked:
			add("fail", "%s: %s is tracked but the workflow would write it; set write_secrets_file: false", u.ID, u.SecretsFile)
		case u.WriteSecretsFile && !g.Ignored(u.SecretsFile):
			add("fail", "%s: %s is not ignored by git; add it to .gitignore before CI writes it", u.ID, u.SecretsFile)
		case !u.WriteSecretsFile && !tracked:
			add("fail", "%s: %s is not tracked, so CI has no secrets file; set write_secrets_file: true", u.ID, u.SecretsFile)
		}
		if upstateHooks && cfg.Upstate != nil {
			if out, err := e.Runner.Run(nil, "git", "-C", root, "ls-files", ".upstate"); err != nil || len(strings.TrimSpace(string(out))) == 0 {
				add("warn", "%s: Upstate hooks are installed but .upstate/ is not committed; CI hooks cannot find the mapping", u.ID)
			}
		}
	}
	if upstateHooks && cfg.Upstate == nil {
		add("ok", "Upstate hooks are installed; CI deploys report nothing to Upstate until you opt in (init --upstate-cli, provision)")
	}

	// GitHub, through the user's gh.
	owner, name := repoSlug(root)
	repo := owner + "/" + name
	ghOK := owner != "" && e.Runner.LookPath("gh") == nil
	if ghOK {
		if _, err := e.Runner.Run(nil, "gh", "auth", "status", "--hostname", "github.com"); err != nil {
			ghOK = false
			add("warn", "gh is not signed in; GitHub checks skipped")
		}
	} else {
		add("warn", "gh or a github.com origin is missing; GitHub checks skipped")
	}
	if ghOK {
		if out, err := e.Runner.Run(nil, "gh", "repo", "view", repo, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name"); err == nil {
			if b := strings.TrimSpace(string(out)); b != cfg.Branch {
				add("fail", "the default branch is %s but the config deploys %s; workflow_run only runs from the default branch's workflow", b, cfg.Branch)
			} else {
				add("ok", "default branch is %s", b)
			}
		}
		p := &provisioner{e: e, repo: repo, cfg: cfg}
		for _, u := range cfg.Units {
			if _, err := e.Runner.Run(nil, "gh", "api", "repos/"+repo+"/environments/"+u.Environment, "--silent"); err != nil {
				add("fail", "%s: GitHub environment %s does not exist; run kamal-autodeploy provision", u.ID, u.Environment)
				continue
			}
			if err := p.checkEnvironment(u.Environment); err != nil {
				add("fail", "%s: %v", u.ID, err)
				continue
			}
			secrets, err := p.names("secret", u.Environment)
			if err != nil {
				add("warn", "%s: cannot list secrets: %v", u.ID, err)
				continue
			}
			want := append([]string{generate.SSHKeySecret}, u.Secrets...)
			var missing []string
			for _, s := range want {
				if !secrets[s] {
					missing = append(missing, s)
				}
			}
			if len(missing) > 0 {
				add("fail", "%s: missing environment secrets: %s", u.ID, strings.Join(missing, ", "))
			} else {
				add("ok", "%s: all %d environment secrets exist (values not read)", u.ID, len(want))
			}
			vars, err := p.variables(u.Environment)
			if err == nil {
				var unpinned []string
				for _, h := range u.SSH.Hosts {
					if !knownHostsCovers(vars[generate.KnownHostsVar], h, u.SSH.Port) {
						unpinned = append(unpinned, h)
					}
				}
				if len(unpinned) > 0 {
					add("fail", "%s: host keys not pinned for %s", u.ID, strings.Join(unpinned, ", "))
				} else {
					add("ok", "%s: host keys pinned for every host", u.ID)
				}
				if cfg.Upstate != nil {
					if vars[generate.UpstateEnvVar] != "" && secrets[generate.UpstateToken] {
						add("ok", "%s: Upstate receipts configured for %s", u.ID, vars[generate.UpstateEnvVar])
					} else {
						add("ok", "%s: Upstate not configured for this unit (optional)", u.ID)
					}
				}
			}
		}
	}
	failed := false
	for _, c := range checks {
		fmt.Fprintf(e.Out, "%-5s %s\n", c.level, c.text)
		if c.level == "fail" {
			failed = true
		}
	}
	if *remote {
		if !ghOK {
			return errors.New("--remote needs gh signed in and a github.com origin")
		}
		u, ok := cfg.Unit(*unitFlag)
		if !ok {
			return errors.New("--remote needs --unit with a unit ID")
		}
		fmt.Fprintf(e.Out, "\nThis dispatches %s on %s (action=doctor, unit=%s): a GitHub-hosted runner loads the CI key and pinned host keys from %s and runs `ssh %s@<host> true`. It deploys nothing and records no deployment.\n", config.WorkflowName, repo, u.ID, u.Environment, u.SSH.User)
		ok, err := confirm(e, "Dispatch it?", *yes)
		if err != nil || !ok {
			return err
		}
		inputs, _ := json.Marshal(map[string]string{"action": "doctor", "unit": u.ID})
		out, err := e.Runner.Run(strings.NewReader(string(inputs)), "gh", "workflow", "run", filepath.Base(config.WorkflowPath), "--repo", repo, "--ref", cfg.Branch, "--json")
		if err != nil {
			return err
		}
		fmt.Fprint(e.Out, string(out))
		fmt.Fprintf(e.Out, "Follow it with: gh run list --workflow %s --repo %s --limit 1\n", filepath.Base(config.WorkflowPath), repo)
	}
	if failed {
		return errors.New("doctor found problems (fail lines above)")
	}
	return nil
}
