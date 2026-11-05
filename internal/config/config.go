// Package config is the checked-in .kamal-autodeploy.yml: the reviewed list of deployment
// units, their path ownership, ordering, commands and GitHub environments. The generated
// workflow is rendered from it; selection in CI reads it again from the deployed commit.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the configuration file at the repository root.
const FileName = ".kamal-autodeploy.yml"

// WorkflowPath is the single generated workflow.
const WorkflowPath = ".github/workflows/kamal-autodeploy.yml"

// WorkflowName is the generated workflow's `name:`.
const WorkflowName = "kamal-autodeploy"

// DeploymentTask marks the GitHub Deployments this tool records, so deployments GitHub
// creates on its own for environment jobs (task "deploy") are never read as a baseline.
const DeploymentTask = "kamal-autodeploy"

// Unclassified policies.
const (
	UnclassifiedDeploy = "deploy" // deploy every unit when a changed file belongs to none
	UnclassifiedStop   = "stop"   // deploy nothing and fail the run until the rules cover it
)

// Registry password sources.
const (
	RegistryGitHubToken = "github_token"
	RegistrySecret      = "secret"
	RegistryNone        = "none"
)

// Kamal install modes.
const (
	KamalBundler = "bundler" // the project's Gemfile.lock pins kamal; bin/ wrappers use bundler
	KamalGem     = "gem"     // gem install kamal -v <kamal_version>
)

type Config struct {
	Version      int        `yaml:"version"`
	Branch       string     `yaml:"branch"`
	ToolVersion  string     `yaml:"tool_version"`
	CI           []Workflow `yaml:"ci"`
	Unclassified string     `yaml:"unclassified"`
	Ignore       []string   `yaml:"ignore,omitempty"`
	Shared       []Shared   `yaml:"shared,omitempty"`
	Units        []Unit     `yaml:"units"`
	Upstate      *Upstate   `yaml:"upstate,omitempty"`
}

// Workflow is a CI workflow whose success on the trusted branch gates deploys.
type Workflow struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Shared lists paths that deploy several units together (for example a schema both read).
type Shared struct {
	Paths []string `yaml:"paths"`
	Units []string `yaml:"units"`
}

type Unit struct {
	ID          string `yaml:"id"`
	Environment string `yaml:"environment"`
	// Command is the project's own Kamal entry point (bin/kamal-rails, bin/kamal or kamal).
	Command string `yaml:"command"`
	// Args are appended after the Kamal subcommand (for example -c config/deploy.yml -d production).
	Args        []string `yaml:"args,omitempty"`
	Config      string   `yaml:"config"`
	Destination string   `yaml:"destination,omitempty"`
	Service     string   `yaml:"service,omitempty"`
	Requires    []string `yaml:"requires"`
	After       []string `yaml:"after,omitempty"`
	Lock        string   `yaml:"lock"`
	Paths       []string `yaml:"paths"`
	// SecretsFile is the Kamal secrets file. WriteSecretsFile is true when it is ignored by git
	// and the workflow writes NAME=$NAME references into it; values come from environment secrets.
	SecretsFile      string   `yaml:"secrets_file"`
	WriteSecretsFile bool     `yaml:"write_secrets_file"`
	Secrets          []string `yaml:"secrets,omitempty"`
	RegistryPassword string   `yaml:"registry_password"`
	Kamal            string   `yaml:"kamal"`
	KamalVersion     string   `yaml:"kamal_version,omitempty"`
	SSH              SSH      `yaml:"ssh"`
}

type SSH struct {
	User  string   `yaml:"user"`
	Port  int      `yaml:"port"`
	Hosts []string `yaml:"hosts"`
}

// Upstate is the optional report-only integration: install a pinned upstate CLI in the
// deploy job so the project's Upstate hooks produce full receipts.
type Upstate struct {
	CLIVersion string `yaml:"cli_version"`
	SHA256     string `yaml:"sha256_linux_amd64"`
}

var (
	idPattern       = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	envPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	tokenPattern    = regexp.MustCompile(`^[A-Za-z0-9_./=:@+,-]+$`)
	secretPattern   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,99}$`)
	hostPattern     = regexp.MustCompile(`^[A-Za-z0-9.:_-]{1,253}$`)
	userPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	branchPattern   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	versionPattern  = regexp.MustCompile(`^(v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?|[0-9a-f]{40})$`)
	semverPattern   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	workflowPattern = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9._-]+\.ya?ml$`)
	namePattern     = regexp.MustCompile(`^[^"'\x00-\x1f\\$` + "`" + `]{1,100}$`)
)

// ValidID is the deployment unit ID shape (also used by workflow job and output names).
func ValidID(id string) bool { return idPattern.MatchString(id) }

// reserved are environment names the deploy job sets itself; a secret with one of these
// names would override the job's own controls (or the runner's).
var reserved = map[string]bool{"ACTION": true, "DEPLOY_SHA": true, "ROLLBACK_VERSION": true, "SECRETS_FILE": true,
	"SSH_KEY": true, "KNOWN_HOSTS": true, "SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "PATH": true, "HOME": true,
	"UPSTATE_HOOK_TOKEN": true, "UPSTATE_HOOK_ENVIRONMENT": true, "UPSTATE_URL": true, "UPSTATE_CLI_VERSION": true, "UPSTATE_CLI_SHA256": true,
	"GOFLAGS": true, "GOTOOLCHAIN": true, "BASELINE": true, "RUN_URL": true, "DEPLOYMENT_ID": true, "OUTCOME": true}

// ReservedName reports whether a secret name collides with the workflow's own environment.
func ReservedName(n string) bool {
	return reserved[n] || strings.HasPrefix(n, "RUNNER_") || strings.HasPrefix(n, "ACTIONS_") || strings.HasPrefix(n, "KAMAL_AUTODEPLOY_")
}

// ValidSecretName is the GitHub secret name shape this tool accepts.
func ValidSecretName(n string) bool { return secretPattern.MatchString(n) }

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes strictly (unknown keys are errors, so a typo never silently disables a rule).
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &c, nil
}

// Validate checks every value that reaches the generated workflow or a shell. It is strict
// on purpose: the workflow embeds these strings.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return errors.New("version must be 1")
	}
	if !branchPattern.MatchString(c.Branch) || strings.Contains(c.Branch, "..") {
		return errors.New("branch is missing or invalid")
	}
	if !versionPattern.MatchString(c.ToolVersion) {
		return errors.New("tool_version must be a release tag (v1.2.3), a Go pseudo-version or a full 40-character commit SHA")
	}
	if c.Unclassified != UnclassifiedDeploy && c.Unclassified != UnclassifiedStop {
		return errors.New("unclassified must be deploy or stop")
	}
	if len(c.CI) == 0 {
		return errors.New("ci must list at least one CI workflow; deploys never run without CI")
	}
	ciNames := map[string]bool{}
	for _, w := range c.CI {
		if !namePattern.MatchString(w.Name) {
			return fmt.Errorf("ci workflow name %q is invalid", w.Name)
		}
		if !workflowPattern.MatchString(w.Path) {
			return fmt.Errorf("ci workflow %q needs a path under .github/workflows", w.Name)
		}
		if w.Path == WorkflowPath || w.Name == WorkflowName {
			return errors.New("the kamal-autodeploy workflow cannot gate itself")
		}
		if ciNames[w.Name] {
			return fmt.Errorf("ci workflow %q is listed twice", w.Name)
		}
		ciNames[w.Name] = true
	}
	if err := validatePatterns("ignore", c.Ignore); err != nil {
		return err
	}
	if len(c.Units) == 0 {
		return errors.New("units must list at least one deployment unit")
	}
	ids := map[string]bool{}
	keys := map[string]string{}
	envs := map[string]string{}
	for _, u := range c.Units {
		if !idPattern.MatchString(u.ID) {
			return fmt.Errorf("unit id %q must be lowercase letters, digits, - or _ (start with a letter)", u.ID)
		}
		if ids[u.ID] {
			return fmt.Errorf("unit id %q is listed twice", u.ID)
		}
		ids[u.ID] = true
		key := strings.ReplaceAll(u.ID, "-", "_")
		if other, ok := keys[key]; ok {
			return fmt.Errorf("unit ids %q and %q collide in workflow output names; rename one", other, u.ID)
		}
		keys[key] = u.ID
		if !envPattern.MatchString(u.Environment) {
			return fmt.Errorf("unit %s: environment is missing or invalid", u.ID)
		}
		if other, ok := envs[strings.ToLower(u.Environment)]; ok {
			return fmt.Errorf("units %s and %s share environment %s; each unit needs its own (its deployed baseline lives there)", other, u.ID, u.Environment)
		}
		envs[strings.ToLower(u.Environment)] = u.ID
		if !tokenPattern.MatchString(u.Command) || strings.HasPrefix(u.Command, "-") {
			return fmt.Errorf("unit %s: command must be one plain path or program name", u.ID)
		}
		for _, a := range u.Args {
			if !tokenPattern.MatchString(a) {
				return fmt.Errorf("unit %s: argument %q contains characters the workflow will not pass", u.ID, a)
			}
		}
		if u.Config != "" && !tokenPattern.MatchString(u.Config) {
			return fmt.Errorf("unit %s: config path is invalid", u.ID)
		}
		if u.Destination != "" && !envPattern.MatchString(u.Destination) {
			return fmt.Errorf("unit %s: destination is invalid", u.ID)
		}
		if len(u.Requires) == 0 {
			return fmt.Errorf("unit %s: requires must name at least one ci workflow", u.ID)
		}
		for _, r := range u.Requires {
			if !ciNames[r] {
				return fmt.Errorf("unit %s requires %q, which is not in ci", u.ID, r)
			}
		}
		if !idPattern.MatchString(u.Lock) {
			return fmt.Errorf("unit %s: lock must be a short lowercase name", u.ID)
		}
		if len(u.Paths) == 0 {
			return fmt.Errorf("unit %s: paths must not be empty", u.ID)
		}
		if err := validatePatterns("unit "+u.ID+" paths", u.Paths); err != nil {
			return err
		}
		if u.SecretsFile == "" || !tokenPattern.MatchString(u.SecretsFile) || strings.Contains(u.SecretsFile, "..") || strings.HasPrefix(u.SecretsFile, "/") {
			return fmt.Errorf("unit %s: secrets_file must be a relative path inside the repository", u.ID)
		}
		seen := map[string]bool{}
		for _, s := range u.Secrets {
			if !secretPattern.MatchString(s) || strings.HasPrefix(s, "GITHUB_") {
				return fmt.Errorf("unit %s: secret name %q is invalid (UPPER_SNAKE_CASE, not GITHUB_*)", u.ID, s)
			}
			if ReservedName(s) {
				return fmt.Errorf("unit %s: %s is used by the deploy workflow itself; rename the secret", u.ID, s)
			}
			if seen[s] {
				return fmt.Errorf("unit %s: secret %s is listed twice", u.ID, s)
			}
			seen[s] = true
		}
		switch u.RegistryPassword {
		case RegistryGitHubToken, RegistrySecret, RegistryNone:
		default:
			return fmt.Errorf("unit %s: registry_password must be github_token, secret or none", u.ID)
		}
		if u.RegistryPassword == RegistrySecret && !seen["KAMAL_REGISTRY_PASSWORD"] {
			return fmt.Errorf("unit %s: registry_password secret needs KAMAL_REGISTRY_PASSWORD in secrets", u.ID)
		}
		if u.RegistryPassword == RegistryGitHubToken && seen["KAMAL_REGISTRY_PASSWORD"] {
			return fmt.Errorf("unit %s: KAMAL_REGISTRY_PASSWORD comes from GITHUB_TOKEN; remove it from secrets", u.ID)
		}
		switch u.Kamal {
		case KamalBundler:
		case KamalGem:
			if !semverPattern.MatchString(u.KamalVersion) {
				return fmt.Errorf("unit %s: kamal gem needs kamal_version such as 2.8.2 (an exact version, never latest)", u.ID)
			}
		default:
			return fmt.Errorf("unit %s: kamal must be bundler or gem", u.ID)
		}
		if !userPattern.MatchString(u.SSH.User) {
			return fmt.Errorf("unit %s: ssh.user is missing or invalid", u.ID)
		}
		if u.SSH.Port < 1 || u.SSH.Port > 65535 {
			return fmt.Errorf("unit %s: ssh.port must be 1-65535", u.ID)
		}
		if len(u.SSH.Hosts) == 0 {
			return fmt.Errorf("unit %s: ssh.hosts must list every host so their keys can be pinned", u.ID)
		}
		for _, h := range u.SSH.Hosts {
			if !hostPattern.MatchString(h) || strings.HasPrefix(h, "-") {
				return fmt.Errorf("unit %s: host %q is invalid", u.ID, h)
			}
		}
	}
	for _, u := range c.Units {
		for _, a := range u.After {
			if !ids[a] || a == u.ID {
				return fmt.Errorf("unit %s: after names unknown unit %q", u.ID, a)
			}
		}
	}
	for i, s := range c.Shared {
		if err := validatePatterns(fmt.Sprintf("shared[%d] paths", i), s.Paths); err != nil {
			return err
		}
		if len(s.Paths) == 0 || len(s.Units) == 0 {
			return fmt.Errorf("shared[%d] needs paths and units", i)
		}
		for _, id := range s.Units {
			if !ids[id] {
				return fmt.Errorf("shared[%d] names unknown unit %q", i, id)
			}
		}
	}
	if _, err := c.Order(); err != nil {
		return err
	}
	if c.Upstate != nil {
		if !semverPattern.MatchString(c.Upstate.CLIVersion) {
			return errors.New("upstate.cli_version must be an exact version such as 0.2.13")
		}
		if !sha256Pattern.MatchString(c.Upstate.SHA256) {
			return errors.New("upstate.sha256_linux_amd64 must be the release archive's SHA-256")
		}
	}
	return nil
}

func validatePatterns(where string, patterns []string) error {
	for _, p := range patterns {
		if err := ValidPattern(p); err != nil {
			return fmt.Errorf("%s: %q: %w", where, p, err)
		}
	}
	return nil
}

// Unit returns the unit with id.
func (c *Config) Unit(id string) (Unit, bool) {
	for _, u := range c.Units {
		if u.ID == id {
			return u, true
		}
	}
	return Unit{}, false
}

// Workflow returns the CI workflow with name.
func (c *Config) Workflow(name string) (Workflow, bool) {
	for _, w := range c.CI {
		if w.Name == name {
			return w, true
		}
	}
	return Workflow{}, false
}

// Predecessors is the set of units a unit's deploy job waits for within one run: its
// explicit `after` units plus the unit just before it, among units sharing its lock, in the
// topological order. Units sharing a host or proxy therefore never deploy in parallel, and
// the inferred edges can never form a cycle with `after`.
func (c *Config) Predecessors(id string) []string {
	out := []string{}
	add := func(v string) {
		for _, x := range out {
			if x == v {
				return
			}
		}
		out = append(out, v)
	}
	u, _ := c.Unit(id)
	for _, a := range u.After {
		add(a)
	}
	order, err := c.Order()
	if err != nil {
		return out
	}
	previous := ""
	for _, x := range order {
		if x == id {
			break
		}
		if other, _ := c.Unit(x); other.Lock == u.Lock {
			previous = x
		}
	}
	if previous != "" {
		add(previous)
	}
	sort.Strings(out)
	return out
}

// Order is a deterministic topological order of units; a cycle is an error.
func (c *Config) Order() ([]string, error) {
	state := map[string]int{}
	out := []string{}
	var visit func(id string, path []string) error
	visit = func(id string, path []string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("units have an ordering cycle: %s", strings.Join(append(path, id), " -> "))
		case 2:
			return nil
		}
		state[id] = 1
		u, _ := c.Unit(id)
		for _, a := range u.After {
			if err := visit(a, append(path, id)); err != nil {
				return err
			}
		}
		state[id] = 2
		out = append(out, id)
		return nil
	}
	for _, u := range c.Units {
		if err := visit(u.ID, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RequiredWorkflows is every CI workflow name any unit requires, in ci order.
func (c *Config) RequiredWorkflows() []string {
	used := map[string]bool{}
	for _, u := range c.Units {
		for _, r := range u.Requires {
			used[r] = true
		}
	}
	out := []string{}
	for _, w := range c.CI {
		if used[w.Name] {
			out = append(out, w.Name)
		}
	}
	return out
}

// Marshal renders the config with a short explanatory header.
func (c *Config) Marshal() ([]byte, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	header := "# kamal-autodeploy configuration: the reviewed plan for merge-to-main deploys.\n" +
		"# Edit it freely, then run `kamal-autodeploy init` to regenerate the workflow.\n" +
		"# Docs: https://github.com/Upstate-run/kamal-autodeploy#configuration\n"
	return append([]byte(header), body...), nil
}
