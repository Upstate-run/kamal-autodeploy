package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/discover"
	"github.com/Upstate-run/kamal-autodeploy/internal/generate"
	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
)

// provision sets up GitHub through the user's own `gh` login. Every change is shown first
// and needs a yes. Secret values travel only on gh's stdin: never argv, logs or files in the
// repository. Existing secrets are never overwritten without --overwrite and a per-secret yes.

type provisioner struct {
	e         *Env
	root      string
	repo      string
	cfg       *config.Config
	yes       bool
	overwrite bool
	noUpstate bool
	imports   []string
	pins      map[string]string // host -> SHA256:fingerprint verified out of band
	tmpDir    string
}

func provisionCommand(e *Env, args []string) error {
	f := newFlags("provision")
	unitFlag := f.String("unit", "", "")
	yes := f.Bool("yes", false, "")
	overwrite := f.Bool("overwrite", false, "")
	noUpstate := f.Bool("no-upstate", false, "")
	var imports, pins multiFlag
	f.Var(&imports, "import", "")
	f.Var(&pins, "host-fingerprint", "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return errors.New("usage: kamal-autodeploy provision [--unit ID] [--import FILE]... [--host-fingerprint HOST=SHA256:...]... [--no-upstate] [--overwrite] [--yes]")
	}
	cfg, root, err := loadRepoConfig(e)
	if err != nil {
		return err
	}
	owner, name := repoSlug(root)
	if owner == "" {
		return errors.New("the origin remote is not a github.com repository")
	}
	p := &provisioner{e: e, root: root, repo: owner + "/" + name, cfg: cfg, yes: *yes, overwrite: *overwrite, noUpstate: *noUpstate, imports: imports, pins: map[string]string{}}
	for _, pin := range pins {
		host, fp, ok := strings.Cut(pin, "=")
		if !ok || !strings.HasPrefix(fp, "SHA256:") {
			return errors.New("--host-fingerprint takes HOST=SHA256:... (as printed by ssh-keygen -lf on the server)")
		}
		p.pins[host] = fp
	}
	if err := e.Runner.LookPath("gh"); err != nil {
		return errors.New("the GitHub CLI (gh) is required; install it and run gh auth login")
	}
	if _, err := e.Runner.Run(nil, "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		return errors.New("gh is not signed in to github.com; run gh auth login")
	}
	fmt.Fprintf(e.Out, "Provisioning %s through your gh login. Nothing reaches Upstate or any server.\n", p.repo)
	units := cfg.Units
	if *unitFlag != "" {
		u, ok := cfg.Unit(*unitFlag)
		if !ok {
			return errors.New("unknown unit " + *unitFlag)
		}
		units = []config.Unit{u}
	}
	for _, u := range units {
		fmt.Fprintf(e.Out, "\n== %s (environment %s)\n", u.ID, u.Environment)
		if err := p.unit(u); err != nil {
			return fmt.Errorf("%s: %w", u.ID, err)
		}
	}
	fmt.Fprintln(e.Out, "\nGitHub provisioning finished. Server login authorization is NOT verified. Install the printed CI public key on each host, then run kamal-autodeploy doctor and kamal-autodeploy doctor --remote --unit <id> before enabling automatic deploys. The remote check needs the workflow on the default branch.")
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func (p *provisioner) gh(stdin []byte, args ...string) ([]byte, error) {
	var in *bytes.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
		return p.e.Runner.Run(in, "gh", args...)
	}
	return p.e.Runner.Run(nil, "gh", args...)
}

func (p *provisioner) unit(u config.Unit) error {
	if err := p.environment(u); err != nil {
		return err
	}
	secrets, err := p.names("secret", u.Environment)
	if err != nil {
		return err
	}
	vars, err := p.variables(u.Environment)
	if err != nil {
		return err
	}
	if err := p.sshKey(u, secrets); err != nil {
		return err
	}
	if err := p.hostKeys(u, vars); err != nil {
		return err
	}
	if err := p.appSecrets(u, secrets); err != nil {
		return err
	}
	if !p.noUpstate {
		if err := p.upstate(u, secrets); err != nil {
			return err
		}
	}
	return nil
}

func (p *provisioner) environment(u config.Unit) error {
	if _, err := p.gh(nil, "api", "repos/"+p.repo+"/environments/"+u.Environment, "--silent"); err == nil {
		if err := p.checkEnvironment(u.Environment); err != nil {
			return err
		}
		fmt.Fprintf(p.e.Out, "  environment %s exists (its protection rules are left as they are)\n", u.Environment)
		return nil
	} else if !strings.Contains(err.Error(), "404") {
		return fmt.Errorf("cannot inspect environment %s: %w", u.Environment, err)
	}
	fmt.Fprintf(p.e.Out, "  will create environment %s, deployable only from branch %s\n", u.Environment, p.cfg.Branch)
	ok, err := confirm(p.e, "  Create it?", p.yes)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("environment is required; nothing else was changed")
		}
		return err
	}
	body := []byte(`{"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}`)
	if _, err := p.gh(body, "api", "-X", "PUT", "repos/"+p.repo+"/environments/"+u.Environment, "--input", "-", "--silent"); err != nil {
		return err
	}
	policy, _ := json.Marshal(map[string]string{"name": p.cfg.Branch, "type": "branch"})
	if _, err := p.gh(policy, "api", "-X", "POST", "repos/"+p.repo+"/environments/"+u.Environment+"/deployment-branch-policies", "--input", "-", "--silent"); err != nil {
		return err
	}
	fmt.Fprintf(p.e.Out, "  created %s (add required reviewers in GitHub settings if you want a manual gate)\n", u.Environment)
	return nil
}

func (p *provisioner) names(kind, env string) (map[string]bool, error) {
	out, err := p.gh(nil, kind, "list", "--env", env, "--repo", p.repo, "--json", "name")
	if err != nil {
		return nil, err
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, errors.New("unexpected gh " + kind + " list output")
	}
	m := map[string]bool{}
	for _, s := range list {
		m[s.Name] = true
	}
	return m, nil
}

func (p *provisioner) variables(env string) (map[string]string, error) {
	out, err := p.gh(nil, "variable", "list", "--env", env, "--repo", p.repo, "--json", "name,value")
	if err != nil {
		return nil, err
	}
	var list []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, errors.New("unexpected gh variable list output")
	}
	m := map[string]string{}
	for _, v := range list {
		m[v.Name] = v.Value
	}
	return m, nil
}

func (p *provisioner) setSecret(env, name string, value []byte) error {
	_, err := p.gh(value, "secret", "set", name, "--env", env, "--repo", p.repo)
	return err
}

func (p *provisioner) setVariable(env, name, value string) error {
	_, err := p.gh([]byte(value), "variable", "set", name, "--env", env, "--repo", p.repo)
	return err
}

// allowOverwrite asks before replacing an existing secret; never with --yes alone.
func (p *provisioner) allowOverwrite(name string, exists bool) (bool, error) {
	if !exists {
		return true, nil
	}
	if !p.overwrite {
		fmt.Fprintf(p.e.Out, "  %s exists; kept (pass --overwrite to be asked about replacing it)\n", name)
		return false, nil
	}
	if !p.e.Interactive {
		fmt.Fprintf(p.e.Out, "  %s exists; kept (replacing needs an interactive yes)\n", name)
		return false, nil
	}
	return confirm(p.e, "  Replace the existing "+name+"?", false)
}

func (p *provisioner) sshKey(u config.Unit, secrets map[string]bool) error {
	ok, err := p.allowOverwrite(generate.SSHKeySecret, secrets[generate.SSHKeySecret])
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(p.e.Out, "  An existing GitHub CI key does not prove server access. Its matching public key must be authorized for the configured SSH user; verify with doctor --remote.")
		return nil
	}
	if err := p.e.Runner.LookPath("ssh-keygen"); err != nil {
		return errors.New("ssh-keygen is required")
	}
	fmt.Fprintf(p.e.Out, "  will generate a dedicated Ed25519 key for CI (never your own key) and store the private half as environment secret %s\n", generate.SSHKeySecret)
	ok, err = confirm(p.e, "  Generate and store it?", p.yes)
	if err != nil || !ok {
		return err
	}
	dir, err := os.MkdirTemp(p.tmpDir, "kamal-autodeploy-key-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "id_ed25519")
	comment := "kamal-autodeploy-ci " + p.repo + " " + u.Environment
	if _, err := p.e.Runner.Run(nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", keyPath); err != nil {
		return err
	}
	private, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	public, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return err
	}
	if err := p.setSecret(u.Environment, generate.SSHKeySecret, private); err != nil {
		return err
	}
	pub := strings.TrimSpace(string(public))
	fmt.Fprintf(p.e.Out, "  stored %s. The private key is deleted locally now.\n", generate.SSHKeySecret)
	fmt.Fprintf(p.e.Out, "  REQUIRED SERVER STEP: authorize this CI public key for the configured SSH user on every host. The server fingerprint does NOT authorize CI login. This tool never SSHes for you:\n    %s\n", pub)
	for _, h := range u.SSH.Hosts {
		fmt.Fprintf(p.e.Out, "    echo %s | ssh -o StrictHostKeyChecking=yes -p %d %s@%s 'umask 077; mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && cat >> ~/.ssh/authorized_keys'\n", shellQuote(pub), u.SSH.Port, u.SSH.User, h)
	}
	fmt.Fprintln(p.e.Out, "  Prefer a dedicated deploy user. Membership in the docker group is effectively root on that host.")
	return nil
}

var fingerprintLine = regexp.MustCompile(`^\d+ (SHA256:[A-Za-z0-9+/]+) `)

func (p *provisioner) hostKeys(u config.Unit, vars map[string]string) error {
	current := vars[generate.KnownHostsVar]
	var missing []string
	for _, h := range u.SSH.Hosts {
		if !knownHostsCovers(current, h, u.SSH.Port) {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintf(p.e.Out, "  %s already pins every host\n", generate.KnownHostsVar)
		return nil
	}
	if err := p.e.Runner.LookPath("ssh-keyscan"); err != nil {
		return errors.New("ssh-keyscan is required")
	}
	lines := strings.TrimSpace(current)
	added := 0
	for _, h := range missing {
		out, err := p.e.Runner.Run(nil, "ssh-keyscan", "-T", "10", "-p", fmt.Sprint(u.SSH.Port), "-t", "ed25519", "--", h)
		candidate := firstKeyLine(out)
		if err != nil || candidate == "" {
			fmt.Fprintf(p.e.Out, "  %s: could not read a host key (%v); skipped\n", h, err)
			continue
		}
		fpOut, err := p.e.Runner.Run(strings.NewReader(candidate+"\n"), "ssh-keygen", "-l", "-E", "sha256", "-f", "-")
		m := fingerprintLine.FindStringSubmatch(strings.TrimSpace(string(fpOut)))
		if err != nil || m == nil {
			fmt.Fprintf(p.e.Out, "  %s: could not fingerprint the scanned key; skipped\n", h)
			continue
		}
		fp := m[1]
		fmt.Fprintf(p.e.Out, "  %s presents ED25519 key %s\n", h, fp)
		fmt.Fprintln(p.e.Out, "  ssh-keyscan is not verification. Compare with the server's own fingerprint, read out of band")
		fmt.Fprintln(p.e.Out, "  (provider console: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub).")
		verified := false
		switch want, ok := p.pins[h]; {
		case ok && want == fp:
			verified = true
			fmt.Fprintln(p.e.Out, "  matches the fingerprint you passed with --host-fingerprint")
		case ok:
			return fmt.Errorf("%s: scanned fingerprint %s does not match --host-fingerprint %s; possible interception, nothing pinned", h, fp, want)
		case p.e.Interactive:
			fmt.Fprint(p.e.Out, "  Type the fingerprint you verified (or press Enter to skip): ")
			typed, _ := bufio.NewReader(p.e.In).ReadString('\n')
			typed = strings.TrimSpace(typed)
			if typed == "" {
				fmt.Fprintln(p.e.Out, "  skipped; nothing pinned for this host")
				continue
			}
			if typed != fp {
				return fmt.Errorf("%s: the fingerprint you typed does not match the scanned key; nothing pinned", h)
			}
			verified = true
		default:
			fmt.Fprintf(p.e.Out, "  not pinned: rerun with --host-fingerprint %s=<fingerprint you verified>\n", h)
		}
		if verified {
			if lines != "" {
				lines += "\n"
			}
			lines += candidate
			added++
		}
	}
	if added == 0 {
		return nil
	}
	if err := p.setVariable(u.Environment, generate.KnownHostsVar, lines+"\n"); err != nil {
		return err
	}
	fmt.Fprintf(p.e.Out, "  pinned %d host key(s) in environment variable %s (public data)\n", added, generate.KnownHostsVar)
	return nil
}

// firstKeyLine returns the first non-comment known_hosts line.
func firstKeyLine(out []byte) string {
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") && len(strings.Fields(l)) == 3 {
			return l
		}
	}
	return ""
}

func knownHostsCovers(known, host string, port int) bool {
	name := host
	if port != 22 {
		name = fmt.Sprintf("[%s]:%d", host, port)
	}
	for _, l := range strings.Split(known, "\n") {
		fields := strings.Fields(l)
		if len(fields) < 3 {
			continue
		}
		for _, n := range strings.Split(fields[0], ",") {
			if n == name {
				return true
			}
		}
	}
	return false
}

// importable reads literal values from ignored, untracked dotenv files. Values with shell
// substitution are never evaluated; the file is never sourced.
func (p *provisioner) importable(u config.Unit) (map[string][]byte, map[string]string, error) {
	values := map[string][]byte{}
	manual := map[string]string{}
	files := append([]string{}, p.imports...)
	if len(files) == 0 && u.WriteSecretsFile {
		if _, err := os.Lstat(filepath.Join(p.root, u.SecretsFile)); err == nil {
			files = append(files, u.SecretsFile)
		}
	}
	g := gitx.Git{Dir: p.root}
	for _, rel := range files {
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			return nil, nil, errors.New("--import takes a path inside the repository")
		}
		if g.Tracked(rel) {
			return nil, nil, errors.New(rel + " is tracked by git; refusing to import from it (it should not hold values)")
		}
		path := filepath.Join(p.root, rel)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, nil, errors.New(rel + " is not a regular file under 1 MiB")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range parseDotenv(raw) {
			if v.literal {
				values[k] = v.value
			} else {
				manual[k] = rel + " computes it (" + v.kind + "); set it yourself: gh secret set " + k + " --env " + u.Environment
			}
		}
	}
	// Rails credentials stay encrypted in the repository; CI needs only the key.
	if _, ok := values["RAILS_MASTER_KEY"]; !ok {
		for _, rel := range []string{"config/credentials/production.key", "config/master.key"} {
			if g.Tracked(rel) {
				continue
			}
			if raw, err := os.ReadFile(filepath.Join(p.root, rel)); err == nil && len(bytes.TrimSpace(raw)) > 0 {
				values["RAILS_MASTER_KEY"] = bytes.TrimSpace(raw)
				delete(manual, "RAILS_MASTER_KEY")
				break
			}
		}
	}
	return values, manual, nil
}

type dotenvValue struct {
	value   []byte
	literal bool
	kind    string
}

var envLine = regexp.MustCompile(`^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)

func parseDotenv(raw []byte) map[string]dotenvValue {
	out := map[string]dotenvValue{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := envLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v := strings.TrimSpace(m[2])
		single := len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\''
		double := len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"'
		switch {
		case single:
			out[m[1]] = dotenvValue{value: []byte(v[1 : len(v)-1]), literal: true}
		case strings.Contains(v, "$(") || strings.Contains(v, "`"):
			out[m[1]] = dotenvValue{kind: "command substitution"}
		case strings.Contains(v, "$"):
			out[m[1]] = dotenvValue{kind: "variable reference"}
		case double:
			inner := v[1 : len(v)-1]
			if strings.Contains(inner, `\`) {
				out[m[1]] = dotenvValue{kind: "escape sequences"}
			} else {
				out[m[1]] = dotenvValue{value: []byte(inner), literal: true}
			}
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			out[m[1]] = dotenvValue{value: []byte(v), literal: true}
		}
	}
	return out
}

func (p *provisioner) appSecrets(u config.Unit, existing map[string]bool) error {
	if len(u.Secrets) == 0 {
		return nil
	}
	values, manual, err := p.importable(u)
	if err != nil {
		return err
	}
	var set, missing []string
	for _, name := range u.Secrets {
		ok, err := p.allowOverwrite(name, existing[name])
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, have := values[name]; have {
			set = append(set, name)
		} else {
			missing = append(missing, name)
		}
	}
	if len(set) > 0 {
		fmt.Fprintf(p.e.Out, "  will set environment secrets from local ignored files (values are not shown): %s\n", strings.Join(set, ", "))
		ok, err := confirm(p.e, "  Set them?", p.yes)
		if err != nil {
			return err
		}
		if ok {
			for _, name := range set {
				if err := p.setSecret(u.Environment, name, values[name]); err != nil {
					return err
				}
			}
			fmt.Fprintf(p.e.Out, "  set %d secret(s)\n", len(set))
		}
	}
	for _, name := range missing {
		if why, ok := manual[name]; ok {
			fmt.Fprintf(p.e.Out, "  %s: %s\n", name, why)
		} else {
			fmt.Fprintf(p.e.Out, "  %s: no local value found; set it yourself: gh secret set %s --env %s --repo %s\n", name, name, u.Environment, p.repo)
		}
	}
	if u.RegistryPassword == config.RegistryGitHubToken {
		fmt.Fprintln(p.e.Out, "  registry: GHCR uses the job's GITHUB_TOKEN; nothing to store")
	}
	return nil
}

func (p *provisioner) upstate(u config.Unit, existing map[string]bool) error {
	m, ok := discover.ReadUpstateMapping(p.root, u.Service, u.Destination)
	if !ok {
		return nil
	}
	if !regexp.MustCompile(`^env_[a-z0-9_]+$`).MatchString(m.EnvironmentID) {
		fmt.Fprintln(p.e.Out, "  Upstate mapping has an unexpected environment ID; skipped")
		return nil
	}
	origin := m.Origin
	if origin == "" {
		origin = "https://upstate.cloud"
	}
	if !regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]+)?$`).MatchString(origin) {
		fmt.Fprintln(p.e.Out, "  Upstate mapping origin is not a plain HTTPS origin; skipped")
		return nil
	}
	fmt.Fprintf(p.e.Out, "  optional: this unit is connected to Upstate (%s, %s).\n", m.EnvironmentID, origin)
	fmt.Fprintln(p.e.Out, "  Storing its hook token lets the Upstate hooks produce full receipts for CI deploys. It is")
	fmt.Fprintln(p.e.Out, "  report-only, scoped to this one environment and revocable in Upstate. Skip it and deploys")
	fmt.Fprintln(p.e.Out, "  still work; Upstate then sees them only as observed version changes.")
	if p.cfg.Upstate == nil {
		fmt.Fprintln(p.e.Out, "  note: the workflow installs the upstate CLI only after kamal-autodeploy init --upstate-cli <version>")
	}
	if err := p.e.Runner.LookPath("upstate"); err != nil {
		fmt.Fprintln(p.e.Out, "  upstate CLI not found on this machine; skipped")
		return nil
	}
	ok, err := p.allowOverwrite(generate.UpstateToken, existing[generate.UpstateToken])
	if err != nil || !ok {
		return err
	}
	ok, err = confirm(p.e, "  Store the Upstate hook token as "+generate.UpstateToken+"?", false)
	if err != nil {
		if !p.e.Interactive {
			fmt.Fprintln(p.e.Out, "  skipped (needs an interactive yes; --yes never opts into Upstate)")
			return nil
		}
		return err
	}
	if !ok {
		return nil
	}
	err = p.e.Piper.Pipe([]string{"upstate", "credential", "hook", m.EnvironmentID}, []string{"UPSTATE_URL=" + origin},
		[]string{"gh", "secret", "set", generate.UpstateToken, "--env", u.Environment, "--repo", p.repo})
	if err != nil {
		return err
	}
	if err := p.setVariable(u.Environment, generate.UpstateEnvVar, m.EnvironmentID); err != nil {
		return err
	}
	if err := p.setVariable(u.Environment, generate.UpstateURLVar, origin); err != nil {
		return err
	}
	fmt.Fprintf(p.e.Out, "  stored %s and set %s, %s\n", generate.UpstateToken, generate.UpstateEnvVar, generate.UpstateURLVar)
	return nil
}

// Existing protections remain untouched; provisioning may only use an environment
// restricted exclusively to this exact deploy branch (no wildcard or tag policies).
func (p *provisioner) checkEnvironment(env string) error {
	path := "repos/" + p.repo + "/environments/" + env
	out, err := p.gh(nil, "api", path)
	if err != nil {
		return err
	}
	var got struct {
		Policy *struct {
			Protected bool `json:"protected_branches"`
			Custom    bool `json:"custom_branch_policies"`
		} `json:"deployment_branch_policy"`
	}
	if json.Unmarshal(out, &got) != nil || got.Policy == nil || got.Policy.Protected || !got.Policy.Custom {
		return fmt.Errorf("environment %s must restrict deployments to branch %s using custom branch policies; existing protections were left unchanged", env, p.cfg.Branch)
	}
	out, err = p.gh(nil, "api", path+"/deployment-branch-policies?per_page=100")
	if err != nil {
		return err
	}
	var policies struct {
		Total int `json:"total_count"`
		Items []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"branch_policies"`
	}
	if json.Unmarshal(out, &policies) != nil || policies.Total != 1 || len(policies.Items) != 1 || policies.Items[0].Name != p.cfg.Branch || policies.Items[0].Type != "branch" {
		return fmt.Errorf("environment %s must allow only branch %s, with no other branch or tag policies", env, p.cfg.Branch)
	}
	return nil
}
