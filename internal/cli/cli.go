// Package cli implements the kamal-autodeploy commands.
package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/discover"
	"github.com/Upstate-run/kamal-autodeploy/internal/generate"
)

// Version is set at release time with -ldflags "-X .../internal/cli.Version=v0.1.0".
var Version = ""

// ToolVersion is what generated workflows pin: the release tag, else the Go module version
// this binary was installed at (go install ...@<commit> gives a pseudo-version).
func ToolVersion() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return ""
}

const usage = `kamal-autodeploy: merge-to-main deploys of the Kamal services a change affects.

Independent open-source project; not affiliated with 37signals or the Kamal project.

Usage:
  kamal-autodeploy plan                      show the discovered plan and the file diff; writes nothing
  kamal-autodeploy init [--dry-run] [--yes]  write .kamal-autodeploy.yml and the workflow
  kamal-autodeploy provision [--unit ID]     set up GitHub environments, the CI SSH key,
                                             pinned host keys and secrets through your gh
  kamal-autodeploy doctor [--remote --unit ID]
                                             check the setup; --remote dispatches a no-op SSH job
  kamal-autodeploy version

Used by the generated workflow:
  kamal-autodeploy select | guard | record
`

type Env struct {
	Dir    string
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	Runner Runner
	Piper  Piper
	// Interactive is true when In is a terminal (confirmations may be asked).
	Interactive bool
	Getenv      func(string) string
}

// Main is the entry point; it returns the process exit code.
func Main(args []string) int {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kamal-autodeploy:", err)
		return 1
	}
	r := osRunner{dir: dir}
	env := &Env{Dir: dir, In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Runner: r, Piper: r, Interactive: isTerminal(os.Stdin), Getenv: os.Getenv}
	if err := Run(env, args); err != nil {
		fmt.Fprintln(os.Stderr, "kamal-autodeploy:", err)
		return 1
	}
	return 0
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func Run(e *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(e.Out, usage)
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(e.Out, usage)
		return nil
	case "version", "--version":
		v := ToolVersion()
		if v == "" {
			v = "(development build)"
		}
		fmt.Fprintln(e.Out, v)
		return nil
	case "plan":
		return initCommand(e, append([]string{"--dry-run"}, args[1:]...))
	case "init":
		return initCommand(e, args[1:])
	case "provision":
		return provisionCommand(e, args[1:])
	case "doctor":
		return doctorCommand(e, args[1:])
	case "select":
		return selectCommand(e, args[1:])
	case "guard":
		return guardCommand(e, args[1:])
	case "verify-hosts":
		return verifyHostsCommand(e, args[1:])
	case "record":
		return recordCommand(e, args[1:])
	}
	return fmt.Errorf("unknown command %q; run kamal-autodeploy --help", args[0])
}

func newFlags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}

var githubRemote = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// repoSlug reads owner/name from the origin remote (github.com only).
func repoSlug(dir string) (string, string) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", ""
	}
	m := githubRemote.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

func repoRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("run kamal-autodeploy inside a git repository")
	}
	return strings.TrimSpace(string(out)), nil
}

func confirm(e *Env, question string, assume bool) (bool, error) {
	if assume {
		return true, nil
	}
	if !e.Interactive {
		return false, errors.New("confirmation needed; rerun in a terminal or pass --yes after reviewing the plan")
	}
	fmt.Fprintf(e.Out, "%s [y/N] ", question)
	line, err := bufio.NewReader(e.In).ReadString('\n')
	if err != nil && line == "" {
		return false, errors.New("no answer; nothing was changed")
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}

// initCommand discovers (or loads) the config, prints the plan and diffs, and writes.
func initCommand(e *Env, args []string) error {
	f := newFlags("init")
	dry := f.Bool("dry-run", false, "")
	yes := f.Bool("yes", false, "")
	force := f.Bool("force", false, "")
	branch := f.String("branch", "main", "")
	toolVersion := f.String("tool-version", "", "")
	rediscover := f.Bool("rediscover", false, "")
	directKamal := f.Bool("direct-kamal", false, "")
	activation := f.String("activation", "", "")
	upstateCLI := f.String("upstate-cli", "", "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return errors.New("usage: kamal-autodeploy init [--dry-run] [--yes] [--branch main] [--tool-version vX.Y.Z] [--rediscover] [--force] [--direct-kamal] [--activation manual|automatic] [--upstate-cli VERSION|off]")
	}
	root, err := repoRoot(e.Dir)
	if err != nil {
		return err
	}
	owner, name := repoSlug(root)
	tv := *toolVersion
	if tv == "" {
		tv = ToolVersion()
	}
	cfgPath := filepath.Join(root, config.FileName)
	existingCfg, cfgErr := os.ReadFile(cfgPath)
	var cfg *config.Config
	var report *discover.Report
	if cfgErr == nil && !*rediscover {
		cfg, err = config.Parse(existingCfg)
		if err != nil {
			return err
		}
		if *toolVersion != "" {
			cfg.ToolVersion = *toolVersion
		}
		fmt.Fprintf(e.Out, "Using the reviewed %s (pass --rediscover to propose a fresh one).\n\n", config.FileName)
	} else {
		report, err = discover.Discover(root, discover.Options{Branch: *branch, Owner: owner, ToolVersion: tv, DirectKamal: *directKamal})
		if err != nil {
			return err
		}
		cfg = report.Config
		// Rediscovery changes application discovery, not deployment activation.
		// Invalid or absent prior configuration must never opt into automation.
		cfg.Activation = config.ActivationManual
		if cfgErr == nil {
			if previous, parseErr := config.Parse(existingCfg); parseErr == nil {
				cfg.Activation = previous.Activation
			}
		}
	}
	if (*activation != "" || *directKamal) && *toolVersion == "" && tv != "" {
		cfg.ToolVersion = tv
	}
	if *activation != "" {
		cfg.Activation = *activation
	}
	if *directKamal {
		// Mark explicit opt-in so validation rejects an incompatible old pin.
		cfg.Activation = cfg.EffectiveActivation()
		for i := range cfg.Units {
			u := &cfg.Units[i]
			u.Command = "kamal"
			u.Args = config.DirectArgs(u.Config, u.Destination)
		}
	}
	if *upstateCLI != "" {
		if err := pinUpstateCLI(e, cfg, *upstateCLI); err != nil {
			return err
		}
	}
	slug := owner + "/" + name
	if owner == "" {
		slug = "(no github.com origin remote)"
	}
	printPlan(e.Out, slug, cfg, report)
	if cfg.ToolVersion == "" {
		return errors.New("this is a development build; pass --tool-version (a release tag or a full commit SHA of kamal-autodeploy) so the workflow installs an exact version")
	}
	if report != nil {
		if blocking := report.Blocking(); len(blocking) > 0 {
			return errors.New("the plan has unsupported shapes (listed above); fix them or edit the proposed config by hand, then rerun")
		}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("the proposed config is not valid yet: %w", err)
	}
	cfgBytes := existingCfg
	if report != nil || *toolVersion != "" || *upstateCLI != "" || *directKamal || *activation != "" {
		if cfgBytes, err = cfg.Marshal(); err != nil {
			return err
		}
	}
	wf, err := generate.Workflow(cfg)
	if err != nil {
		return err
	}
	wfPath := filepath.Join(root, config.WorkflowPath)
	existingWf, wfErr := os.ReadFile(wfPath)
	if wfErr != nil {
		existingWf = nil
	}
	state := generate.Classify(existingWf, wf)
	type change struct {
		path         string
		before, data []byte
	}
	var changes []change
	if cfgErr != nil || string(existingCfg) != string(cfgBytes) {
		before := existingCfg
		if cfgErr != nil {
			before = nil
		}
		changes = append(changes, change{config.FileName, before, cfgBytes})
	}
	switch state {
	case generate.Foreign:
		return fmt.Errorf("%s exists and was not generated by kamal-autodeploy; refusing to overwrite it. Move it aside and rerun", config.WorkflowPath)
	case generate.HandEdited:
		if !*force {
			fmt.Fprint(e.Out, unifiedDiff(config.WorkflowPath, existingWf, wf))
			return fmt.Errorf("%s was edited by hand; refusing to overwrite it. Keep your edits (and keep it in sync yourself), or rerun with --force to replace it with the diff above", config.WorkflowPath)
		}
		changes = append(changes, change{config.WorkflowPath, existingWf, wf})
	case generate.Absent, generate.Outdated:
		changes = append(changes, change{config.WorkflowPath, existingWf, wf})
	}
	fmt.Fprintln(e.Out, "Files")
	if len(changes) == 0 {
		fmt.Fprintln(e.Out, "  up to date; nothing to write")
		return nil
	}
	for _, c := range changes {
		verb := "update"
		if c.before == nil {
			verb = "create"
		}
		fmt.Fprintf(e.Out, "  %s %s\n", verb, c.path)
	}
	fmt.Fprintln(e.Out)
	for _, c := range changes {
		if c.before != nil {
			fmt.Fprint(e.Out, unifiedDiff(c.path, c.before, c.data))
		} else {
			fmt.Fprintf(e.Out, "+++ %s (new, %d lines)\n", c.path, strings.Count(string(c.data), "\n"))
		}
	}
	if *dry {
		fmt.Fprintln(e.Out, "\nDry run: nothing was written.")
		return nil
	}
	ok, err := confirm(e, "Write these files?", *yes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(e.Out, "Nothing was written.")
		return nil
	}
	for _, c := range changes {
		if err := writeFile(filepath.Join(root, c.path), c.data); err != nil {
			return err
		}
	}
	fmt.Fprintln(e.Out, "Written. Nothing was deployed. Next: review and commit both files, then run kamal-autodeploy provision.")
	if !cfg.Automatic() {
		fmt.Fprintln(e.Out, "Manual activation: publish the workflow on the default branch, authorize the CI key, run doctor --remote, then dispatch a manual deploy. After verification, run init --activation automatic and review/commit both files to enable CI-triggered deployment.")
	}
	return nil
}

func writeFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New(path + " is a symlink or not a regular file; refusing to replace it")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kamal-autodeploy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func printPlan(out io.Writer, slug string, c *config.Config, r *discover.Report) {
	fmt.Fprintf(out, "kamal-autodeploy plan for %s (deploy branch %s)\n\n", slug, c.Branch)
	fmt.Fprintf(out, "Activation: %s\n\n", c.EffectiveActivation())
	fmt.Fprintln(out, "CI gates (a unit deploys only after all of its required workflows pass on the branch)")
	for _, w := range c.CI {
		filter := "every push"
		if r != nil {
			for _, d := range r.Workflows {
				if d.Path == w.Path && len(d.PathFilters) > 0 {
					filter = "path-filtered: may not run for every commit"
				}
			}
		}
		fmt.Fprintf(out, "  %-20s %-40s %s\n", w.Name, w.Path, filter)
	}
	if len(c.CI) == 0 {
		fmt.Fprintln(out, "  (none found)")
	}
	order, err := c.Order()
	if err != nil {
		order = nil
		for _, u := range c.Units {
			order = append(order, u.ID)
		}
	}
	fmt.Fprintln(out, "\nDeployment units (in deploy order)")
	for _, id := range order {
		u, _ := c.Unit(id)
		fmt.Fprintf(out, "  %s  ->  GitHub environment %s\n", u.ID, u.Environment)
		args := strings.Join(u.Args, " ")
		if args != "" {
			args = " " + args
		}
		command := u.Command
		if command == "kamal" && u.Kamal == config.KamalBundler {
			command = "bundle exec kamal"
		}
		fmt.Fprintf(out, "    deploy      %s deploy%s --version=<commit>\n", command, args)
		fmt.Fprintf(out, "    rollback    %s rollback <version>%s\n", command, args)
		cfg := u.Config
		if u.Destination != "" {
			cfg += " (destination " + u.Destination + ")"
		}
		fmt.Fprintf(out, "    config      %s\n", cfg)
		fmt.Fprintf(out, "    kamal       %s\n", kamalLabel(u))
		hosts := []string{}
		for _, h := range u.SSH.Hosts {
			hosts = append(hosts, fmt.Sprintf("%s@%s:%d", u.SSH.User, h, u.SSH.Port))
		}
		fmt.Fprintf(out, "    hosts       %s (lock %s)\n", orNone(strings.Join(hosts, ", ")), u.Lock)
		fmt.Fprintf(out, "    requires    %s\n", orNone(strings.Join(u.Requires, ", ")))
		if p := c.Predecessors(u.ID); len(p) > 0 {
			fmt.Fprintf(out, "    after       %s\n", strings.Join(p, ", "))
		}
		fmt.Fprintf(out, "    paths       %s\n", strings.Join(u.Paths, "  "))
		mode := "tracked, used as is"
		if u.WriteSecretsFile {
			mode = "written in CI as NAME=$NAME references"
		}
		fmt.Fprintf(out, "    secrets     %s (%s): %s\n", u.SecretsFile, mode, orNone(strings.Join(u.Secrets, ", ")))
		fmt.Fprintf(out, "    registry    %s\n", registryLabel(u.RegistryPassword))
		fmt.Fprintf(out, "    concurrency one run at a time (group %s, cancel-in-progress false)\n", generate.ConcurrencyPrefix)
		if r != nil {
			for _, info := range r.Units {
				if info.ID == u.ID && info.UpstateEnv != "" {
					fmt.Fprintf(out, "    upstate     Upstate environment %s (optional receipts; see provision)\n", info.UpstateEnv)
				}
			}
		}
	}
	for _, s := range c.Shared {
		fmt.Fprintf(out, "\nShared: %s deploy %s\n", strings.Join(s.Paths, ", "), strings.Join(s.Units, " and "))
	}
	fmt.Fprintf(out, "\nIgnored (never deploy): %s\n", orNone(strings.Join(c.Ignore, ", ")))
	if c.Unclassified == config.UnclassifiedDeploy {
		fmt.Fprintln(out, "Files no unit claims: deploy every unit (never skipped)")
	} else {
		fmt.Fprintln(out, "Files no unit claims: stop and fail the run until the rules cover them")
	}
	if r == nil {
		fmt.Fprintln(out)
		return
	}
	for _, level := range []struct{ key, title string }{
		{discover.Decision, "Review: conservative defaults applied"},
		{discover.Unknown, "Unknown (could not be determined without running project code)"},
		{discover.Unsupported, "Unsupported (fix before using)"},
		{discover.Info, "Notes"},
	} {
		var lines []string
		for _, n := range r.Notes {
			if n.Level != level.key {
				continue
			}
			prefix := ""
			if n.Unit != "" {
				prefix = n.Unit + ": "
			}
			lines = append(lines, "  - "+prefix+n.Text)
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n%s\n%s\n", level.title, strings.Join(lines, "\n"))
	}
	fmt.Fprintln(out)
}

func kamalLabel(u config.Unit) string {
	if u.Kamal == config.KamalBundler {
		return "from Gemfile.lock (bundler)"
	}
	return "gem install kamal -v " + u.KamalVersion
}

func registryLabel(r string) string {
	switch r {
	case config.RegistryGitHubToken:
		return "GHCR with the job's GITHUB_TOKEN (packages: write on this job only)"
	case config.RegistryNone:
		return "no registry password"
	}
	return "KAMAL_REGISTRY_PASSWORD environment secret"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// MinUpstateCLI is the first upstate CLI that reads UPSTATE_HOOK_TOKEN in CI.
const MinUpstateCLI = "0.2.13"

var semver = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)

func versionLess(a, b string) bool {
	ma, mb := semver.FindStringSubmatch(a), semver.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		x, y := atoi(ma[i]), atoi(mb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// pinUpstateCLI records an exact upstate CLI release and its linux_amd64 archive checksum,
// read from that release's checksums.txt through gh. "off" removes the integration.
func pinUpstateCLI(e *Env, cfg *config.Config, version string) error {
	if version == "off" {
		cfg.Upstate = nil
		return nil
	}
	version = strings.TrimPrefix(version, "v")
	if !semver.MatchString(version) {
		return errors.New("--upstate-cli takes an exact version such as " + MinUpstateCLI)
	}
	if versionLess(version, MinUpstateCLI) {
		return fmt.Errorf("upstate CLI %s cannot report from CI; it needs %s or newer", version, MinUpstateCLI)
	}
	out, err := e.Runner.Run(nil, "gh", "release", "download", "v"+version, "--repo", "Upstate-run/cli-releases", "--pattern", "checksums.txt", "--output", "-")
	if err != nil {
		return fmt.Errorf("could not read upstate CLI %s checksums from Upstate-run/cli-releases: %w", version, err)
	}
	archive := "upstate_" + version + "_linux_amd64.tar.gz"
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == archive && regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(f[0]) {
			cfg.Upstate = &config.Upstate{CLIVersion: version, SHA256: f[0]}
			return nil
		}
	}
	return errors.New("checksums.txt of upstate CLI " + version + " has no " + archive)
}
