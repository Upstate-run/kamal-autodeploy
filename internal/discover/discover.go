package discover

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
)

// Note levels.
const (
	Info        = "info"
	Decision    = "decision"    // a conservative default was applied; review it
	Unknown     = "unknown"     // could not be determined statically
	Unsupported = "unsupported" // this shape is not handled; the plan cannot be used as is
)

type Note struct {
	Level string
	Unit  string
	Text  string
}

// UnitInfo is discovery detail the plan prints but the config does not store.
type UnitInfo struct {
	ID                    string
	Image                 string
	Dynamic               bool
	Destination           string
	Base, Overlay         string
	Context               string
	Wrapper               string
	PrivateHosts          []string
	HostsUnknown          bool
	EndpointUnknown       bool
	SSHUserUnknown        bool
	AccessorySecret       []string
	SecretLines           []SecretLine // from a tracked secrets file
	SecretsFile           string
	CommonSecretsFile     string
	RegistryServer        string
	RegistryServerUnknown bool
	SecretsPathUnknown    bool
	SecretIssues          []Note
	RequiredSecrets       []string // actual Kamal inputs before registry-policy filtering
	SecretsTracked        bool
	UpstateEnv            string
	UpstateOrigin         string
}

type Report struct {
	Config    *config.Config
	Units     []UnitInfo
	Workflows []Workflow
	Notes     []Note
}

// Blocking is true when the plan has an unsupported shape or no valid config.
func (r *Report) Blocking() []Note {
	var out []Note
	for _, n := range r.Notes {
		if n.Level == Unsupported {
			out = append(out, n)
		}
	}
	return out
}

type Options struct {
	DirectKamal bool
	Branch      string
	Owner       string // repository owner, for GHCR GITHUB_TOKEN eligibility
	ToolVersion string
}

var idClean = regexp.MustCompile(`[^a-z0-9_-]+`)

func unitID(s string) string {
	s = idClean.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-_")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "app-" + s
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.TrimRight(s, "-_")
}

// Discover proposes a config. It is deterministic for a given tree.
func Discover(root string, opts Options) (*Report, error) {
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	r := &Report{}
	kcs, notes := findKamalConfigs(root)
	r.Notes = append(r.Notes, notes...)
	if len(kcs) == 0 {
		return r, errors.New("no Kamal config found (config/deploy*.yml or deploy*.yml)")
	}
	wfs, wnotes := findWorkflows(root, opts.Branch)
	r.Workflows = wfs
	r.Notes = append(r.Notes, wnotes...)
	cfg := &config.Config{Version: 1, Branch: opts.Branch, ToolVersion: opts.ToolVersion, Unclassified: config.UnclassifiedDeploy,
		Ignore: []string{"docs/**", "*.md", "LICENSE", "LICENSE.*", ".github/ISSUE_TEMPLATE/**", ".github/PULL_REQUEST_TEMPLATE*"}}
	r.Notes = append(r.Notes, Note{Level: Decision, Text: "ignore covers docs/**, root-level *.md and LICENSE only; Markdown elsewhere may be served by the app, so it still deploys"})

	// CI gates: workflows that run on every push to the deploy branch.
	var unconditional []string
	for _, w := range wfs {
		if w.PullRequestTarget {
			r.Notes = append(r.Notes, Note{Level: Info, Text: w.Path + " uses pull_request_target; it is never used as a deploy gate and the deploy workflow does not trust its artifacts"})
		}
		if w.Unparsed || !w.PushBranch {
			continue
		}
		cfg.CI = append(cfg.CI, config.Workflow{Name: w.Name, Path: w.Path})
		if len(w.PathFilters) == 0 {
			unconditional = append(unconditional, w.Name)
		}
	}
	if len(cfg.CI) == 0 {
		r.Notes = append(r.Notes, Note{Level: Unsupported, Text: "no workflow runs on push to " + opts.Branch + "; deploys only follow successful CI, so add a CI workflow first"})
	} else if len(unconditional) == 0 {
		r.Notes = append(r.Notes, Note{Level: Unsupported, Text: "every CI workflow is path-filtered; at least one must run on every push to " + opts.Branch})
	}

	lockedKamal := kamalFromGemfile(root)
	hasBinKamal := executable(filepath.Join(root, "bin", "kamal"))
	git := func(args ...string) bool {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		return cmd.Run() == nil
	}
	tracked := func(p string) bool { return git("ls-files", "--error-unmatch", "--", p) }
	ignored := func(p string) bool { return git("check-ignore", "-q", "--no-index", "--", p) }

	multi := len(kcs) > 1
	usedIDs := map[string]bool{}
	type hostSet struct {
		id    string
		hosts []string
	}
	var units []config.Unit
	var infos []UnitInfo
	for _, kc := range kcs {
		info := UnitInfo{Base: kc.Base, Overlay: kc.Overlay, Destination: kc.Destination, Dynamic: kc.Dynamic, Image: str(kc.Merged["image"])}
		cfgPath := kc.Base
		if kc.Overlay != "" {
			cfgPath = kc.Overlay
		}
		svc := kc.service()
		info.Wrapper = wrapperFor(root, kc.Base)
		id := ""
		switch {
		case info.Wrapper != "" && strings.HasPrefix(filepath.Base(info.Wrapper), "kamal-"):
			id = unitID(strings.TrimPrefix(filepath.Base(info.Wrapper), "kamal-"))
		case kc.Destination != "" && len(kcs) > 1 && allSameBase(kcs):
			id = unitID(kc.Destination)
		case svc != "":
			id = unitID(svc)
		default:
			id = unitID(strings.TrimSuffix(filepath.Base(kc.Base), ".yml"))
		}
		if kc.Destination != "" && !allSameBase(kcs) {
			id = unitID(id + "-" + kc.Destination)
		}
		for usedIDs[id] {
			id = unitID(id + "-2")
		}
		usedIDs[id] = true
		info.ID = id
		if svc == "" {
			r.Notes = append(r.Notes, Note{Level: Unknown, Unit: id, Text: cfgPath + ": service is templated or missing"})
		}

		u := config.Unit{ID: id, Config: kc.Base, Destination: kc.Destination, Service: svc, Lock: "deploy"}
		envName := "production"
		if kc.Destination != "" {
			envName = kc.Destination
		}
		if multi && !(kc.Destination != "" && allSameBase(kcs)) {
			envName = envName + "-" + id
		}
		u.Environment = envName

		if opts.DirectKamal {
			u.Command = "kamal"
			u.Args = config.DirectArgs(kc.Base, kc.Destination)
			r.Notes = append(r.Notes, Note{Level: Info, Unit: id, Text: "CI invokes Kamal directly with explicit config/destination; the laptop wrapper is not executed"})
		} else {
			// Command: the project's own wrapper first, then bin/kamal, then kamal.
			switch {
			case info.Wrapper != "":
				u.Command = info.Wrapper
				r.Notes = append(r.Notes, Note{Level: Info, Unit: id, Text: fmt.Sprintf("uses the project's wrapper %s (it selects %s itself)", info.Wrapper, kc.Base)})
			case hasBinKamal:
				u.Command = "bin/kamal"
			default:
				u.Command = "kamal"
			}
			if info.Wrapper == "" && kc.Base != "config/deploy.yml" {
				u.Args = append(u.Args, "-c", kc.Base)
			}
			if kc.Destination != "" {
				u.Args = append(u.Args, "-d", kc.Destination)
				if info.Wrapper != "" {
					r.Notes = append(r.Notes, Note{Level: Unknown, Unit: id, Text: "wrapper " + info.Wrapper + " combined with destination " + kc.Destination + ": confirm the wrapper passes -d through"})
				}
			}
		}
		switch {
		case lockedKamal != "" && (u.Command != "kamal" || opts.DirectKamal):
			u.Kamal = config.KamalBundler
		case lockedKamal != "":
			u.Kamal, u.KamalVersion = config.KamalGem, lockedKamal
		default:
			u.Kamal = config.KamalGem
			mv := str(kc.Merged["minimum_version"])
			if regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(mv) {
				u.KamalVersion = mv
				r.Notes = append(r.Notes, Note{Level: Decision, Unit: id, Text: "installs kamal " + mv + " (the config's minimum_version) with gem install; pin another exact version in kamal_version if you prefer"})
			} else {
				r.Notes = append(r.Notes, Note{Level: Unsupported, Unit: id, Text: "kamal version unknown (no Gemfile.lock entry, no minimum_version); set kamal_version"})
			}
		}

		// Hosts and SSH.
		hosts, unknownHosts := kc.hosts()
		info.HostsUnknown = unknownHosts
		if unknownHosts {
			// Kamal's SSH accepts unknown host keys by default, so an unlisted host would go unpinned.
			r.Notes = append(r.Notes, Note{Level: Unsupported, Unit: id, Text: "some hosts are templated (ERB) or not a static list; write .kamal-autodeploy.yml by hand listing every host in ssh.hosts (so each key is pinned), then rerun init"})
		}
		for _, h := range hosts {
			if privateHost(h) {
				info.PrivateHosts = append(info.PrivateHosts, h)
			}
		}
		if len(info.PrivateHosts) > 0 {
			r.Notes = append(r.Notes, Note{Level: Unsupported, Unit: id, Text: "private or Tailscale-style hosts (" + strings.Join(info.PrivateHosts, ", ") + ") are not reachable from GitHub-hosted runners; see README \"Private hosts\""})
		}
		ssh := mapping(kc.Merged["ssh"])
		u.SSH = config.SSH{User: "root", Port: 22, Hosts: hosts}
		if user := str(ssh["user"]); user != "" {
			u.SSH.User = user
		} else if ssh["user"] != nil {
			info.SSHUserUnknown = true
			r.Notes = append(r.Notes, Note{Level: Unknown, Unit: id, Text: "ssh.user is templated; set ssh.user"})
		}
		if value, exists := ssh["port"]; exists {
			if port, ok := value.(int); ok && port > 0 && port <= 65535 {
				u.SSH.Port = port
			} else {
				info.EndpointUnknown = true
				r.Notes = append(r.Notes, Note{Level: Unsupported, Unit: id, Text: "ssh.port is unresolved or invalid; use a literal port before enabling CI"})
			}
		}
		if ssh["proxy"] != nil || ssh["proxy_command"] != nil {
			info.EndpointUnknown = true
			r.Notes = append(r.Notes, Note{Level: Unsupported, Unit: id, Text: "ssh.proxy (jump host) is not supported by the generated workflow yet"})
		}

		// Build context and path ownership.
		builder := mapping(kc.Merged["builder"])
		ctx := str(builder["context"])
		if builder["context"] != nil && ctx == "" {
			r.Notes = append(r.Notes, Note{Level: Unknown, Unit: id, Text: "builder.context is templated; path ownership defaults to the whole repository"})
		}
		ctx = strings.Trim(strings.TrimPrefix(ctx, "./"), "/")
		if ctx == "" {
			ctx = "."
		}
		info.Context = ctx
		if ctx == "." {
			u.Paths = []string{"**"}
		} else {
			u.Paths = []string{ctx + "/**"}
		}
		u.Paths = append(u.Paths, dockerignoreNegations(root, ctx)...)
		own := []string{kc.Base}
		if kc.Overlay != "" {
			own = append(own, kc.Overlay)
		}
		if info.Wrapper != "" {
			own = append(own, info.Wrapper)
		}
		if df := str(builder["dockerfile"]); df != "" {
			own = append(own, strings.TrimPrefix(df, "./"))
		} else if ctx == "." {
			own = append(own, "Dockerfile")
		} else {
			own = append(own, ctx+"/Dockerfile")
		}

		// Secrets.
		secretsFile := str(kc.Merged["secrets_path"])
		info.SecretsPathUnknown = kc.Merged["secrets_path"] != nil && secretsFile == ""
		if kc.Merged["secrets_path"] != nil && secretsFile == "" {
			r.Notes = append(r.Notes, Note{Level: Unknown, Unit: id, Text: "secrets_path is templated; set secrets_file"})
		}
		if secretsFile == "" {
			secretsFile = ".kamal/secrets"
		}
		if !info.SecretsPathUnknown {
			info.CommonSecretsFile = secretsFile + "-common"
		}
		if kc.Destination != "" {
			secretsFile += "." + kc.Destination
		}
		u.SecretsFile = secretsFile
		info.SecretsFile = secretsFile
		own = append(own, secretsFile, secretsFile+".example")
		for _, p := range own {
			if config.ValidPattern(p) == nil {
				u.Paths = append(u.Paths, p)
			}
		}
		secretNote := func(level, text string) {
			n := Note{Level: level, Unit: id, Text: text}
			r.Notes = append(r.Notes, n)
			info.SecretIssues = append(info.SecretIssues, n)
		}
		deploySecrets, accessorySecrets, unknownSecrets := kc.secretNames()
		info.RequiredSecrets = append([]string(nil), deploySecrets...)
		info.AccessorySecret = accessorySecrets
		if unknownSecrets {
			secretNote(Unknown, "some secret names are templated; add them to secrets")
		}
		names := deploySecrets
		for _, candidate := range []string{secretsFile + ".example", secretsFile} {
			if !tracked(candidate) && candidate == secretsFile && !ignored(candidate) {
				continue // an untracked file that is not ignored is not read at all
			}
			lines, err := ReadSecretNames(filepath.Join(root, candidate))
			if err != nil {
				continue
			}
			for _, l := range lines {
				names = appendUnique(names, l.Name)
			}
		}
		if tracked(secretsFile) {
			info.SecretsTracked = true
			lines, _ := ReadSecretNames(filepath.Join(root, secretsFile))
			info.SecretLines = lines
			for _, l := range lines {
				switch l.Kind {
				case "command":
					r.Notes = append(r.Notes, Note{Level: Decision, Unit: id, Text: fmt.Sprintf("%s: %s runs a command; in CI it must read $%s instead (for example RAILS_MASTER_KEY=$RAILS_MASTER_KEY), or the deploy fails", secretsFile, l.Name, l.Name)})
				case "literal":
					r.Notes = append(r.Notes, Note{Level: Decision, Unit: id, Text: fmt.Sprintf("%s is tracked and %s has a literal value; it is used as is, not provisioned", secretsFile, l.Name)})
				case "interpolated":
					r.Notes = append(r.Notes, Note{Level: Unknown, Unit: id, Text: fmt.Sprintf("%s: %s interpolates other variables; provision what it references", secretsFile, l.Name)})
				}
			}
		} else {
			u.WriteSecretsFile = true
			r.Notes = append(r.Notes, Note{Level: Info, Unit: id, Text: secretsFile + " is not tracked; the deploy job writes NAME=$NAME references into it and values come from the " + u.Environment + " environment secrets"})
		}
		if len(accessorySecrets) > 0 {
			r.Notes = append(r.Notes, Note{Level: Info, Unit: id, Text: "accessory secrets (" + strings.Join(accessorySecrets, ", ") + ") are not provisioned: deploy and rollback never boot accessories"})
		}

		// Registry.
		registry := mapping(kc.Merged["registry"])
		server, user := strings.ToLower(str(registry["server"])), str(registry["username"])
		info.RegistryServer = server
		info.RegistryServerUnknown = registry["server"] != nil && server == ""
		if server == "" && !info.RegistryServerUnknown {
			info.RegistryServer = "docker.io"
		}
		switch {
		case server == "" && registry["server"] == nil && len(registry) == 0:
			info.RequiredSecrets = appendUnique(info.RequiredSecrets, "KAMAL_REGISTRY_PASSWORD")
			u.RegistryPassword = config.RegistrySecret
			r.Notes = append(r.Notes, Note{Level: Unknown, Unit: id, Text: "no registry block; Kamal defaults to Docker Hub with KAMAL_REGISTRY_PASSWORD"})
		case strings.HasPrefix(server, "localhost") || strings.HasPrefix(server, "127.0.0.1"):
			u.RegistryPassword = config.RegistryNone
		case server == "ghcr.io" && opts.Owner != "" && strings.EqualFold(user, opts.Owner):
			u.RegistryPassword = config.RegistryGitHubToken
			r.Notes = append(r.Notes, Note{Level: Decision, Unit: id, Text: "GHCR images are pushed with the job's GITHUB_TOKEN (packages: write on the deploy job only). If the package is not linked to this repository, grant it Actions access or switch registry_password to secret"})
		default:
			u.RegistryPassword = config.RegistrySecret
		}
		var kept []string
		for _, n := range names {
			if n == "KAMAL_REGISTRY_PASSWORD" && u.RegistryPassword == config.RegistryGitHubToken {
				continue
			}
			if config.ReservedName(n) {
				secretNote(Unsupported, "secret "+n+" has a name the deploy workflow uses itself; rename it in the Kamal config")
				continue
			}
			if !config.ValidSecretName(n) || strings.HasPrefix(n, "GITHUB_") {
				secretNote(Unknown, "secret "+n+" is not a valid GitHub secret name; rename it or provide it another way")
				continue
			}
			kept = append(kept, n)
		}
		if u.RegistryPassword == config.RegistrySecret {
			kept = appendUnique(kept, "KAMAL_REGISTRY_PASSWORD")
		}
		sort.Strings(kept)
		u.Secrets = kept

		// Required CI.
		u.Requires = append([]string{}, unconditional...)
		for _, w := range wfs {
			if !w.PushBranch || len(w.PathFilters) == 0 || w.Unparsed {
				continue
			}
			if ctx != "." && filtersCover(w.PathFilters, ctx) {
				u.Requires = append(u.Requires, w.Name)
				r.Notes = append(r.Notes, Note{Level: Decision, Unit: id, Text: fmt.Sprintf("requires %q too: it is path-filtered and its filter covers %s/", w.Name, ctx)})
			} else {
				r.Notes = append(r.Notes, Note{Level: Decision, Unit: id, Text: fmt.Sprintf("does not require %q (it only runs for some paths); add it to requires if it must gate this unit", w.Name)})
			}
		}

		// Upstate mapping (committed .upstate/*.json): only its public environment ID.
		if m, ok := ReadUpstateMapping(root, svc, kc.Destination); ok {
			info.UpstateEnv, info.UpstateOrigin = m.EnvironmentID, m.Origin
			cfg.Ignore = appendUnique(cfg.Ignore, ".upstate/**")
		}
		units = append(units, u)
		infos = append(infos, info)
	}

	// Locks: units that share any host deploy one at a time (shared kamal-proxy).
	groups := map[int][]int{}
	parent := make([]int, len(units))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	for i := range units {
		for j := i + 1; j < len(units); j++ {
			if overlap(units[i].SSH.Hosts, units[j].SSH.Hosts) || len(units[i].SSH.Hosts) == 0 || len(units[j].SSH.Hosts) == 0 {
				parent[find(j)] = find(i)
			}
		}
	}
	for i := range units {
		groups[find(i)] = append(groups[find(i)], i)
	}
	for _, members := range groups {
		lock := "deploy"
		if len(units[members[0]].SSH.Hosts) > 0 {
			lock = unitID("host-" + units[members[0]].SSH.Hosts[0])
		}
		for _, i := range members {
			units[i].Lock = lock
		}
		if len(members) > 1 {
			ids := []string{}
			for _, i := range members {
				ids = append(ids, units[i].ID)
			}
			r.Notes = append(r.Notes, Note{Level: Info, Text: "units " + strings.Join(ids, ", ") + " share a host (and kamal-proxy); they deploy one at a time, in config order"})
		}
	}

	// Root-context units contain other units' build contexts unless .dockerignore drops them.
	for i := range units {
		if infos[i].Context != "." {
			continue
		}
		for j := range units {
			if i == j {
				continue
			}
			for _, p := range units[j].Paths {
				if strings.HasPrefix(p, "!") || p == "**" {
					continue
				}
				if p == units[j].Config || p == infos[j].Wrapper || p == infos[j].Overlay || strings.HasPrefix(p, units[j].SecretsFile) {
					units[i].Paths = appendUnique(units[i].Paths, "!"+p)
				}
			}
			if infos[j].Context != "." && config.Match(units[i].Paths, infos[j].Context+"/x") {
				r.Notes = append(r.Notes, Note{Level: Decision, Unit: units[i].ID, Text: fmt.Sprintf("%s/ is inside %s's build context and .dockerignore does not exclude it, so %s-only changes deploy %s too. If %s's image does not use it, add \"!%s/**\" to %s's paths", infos[j].Context, units[i].ID, units[j].ID, units[i].ID, units[i].ID, infos[j].Context, units[i].ID)})
			}
		}
	}

	// Shared files: Kamal hooks and the shared bin/kamal deploy every unit.
	if multi {
		r.Notes = append(r.Notes, Note{Level: Decision, Text: "add contracts both units depend on (for example db/schema.rb, db/migrate/**) to shared; discovery cannot infer them"})
		// Schema owner: a root-context unit in a repository with db/migrate deploys first.
		owner := -1
		if dirExists(filepath.Join(root, "db", "migrate")) {
			for i := range units {
				if infos[i].Context == "." {
					owner = i
					break
				}
			}
		}
		if owner >= 0 {
			for i := range units {
				if i != owner && units[i].Lock == units[owner].Lock {
					units[i].After = appendUnique(units[i].After, units[owner].ID)
				}
			}
			if owner != 0 {
				units[0], units[owner] = units[owner], units[0]
				infos[0], infos[owner] = infos[owner], infos[0]
			}
			r.Notes = append(r.Notes, Note{Level: Decision, Unit: units[0].ID, Text: units[0].ID + " owns db/migrate, so units on its host deploy after it"})
		}
	}
	if multi {
		var ids []string
		for _, u := range units {
			ids = append(ids, u.ID)
		}
		cfg.Shared = append(cfg.Shared, config.Shared{Paths: []string{".kamal/hooks/**", "bin/kamal"}, Units: ids})
	}
	cfg.Units = units
	r.Config = cfg
	r.Units = infos
	return r, nil
}

func allSameBase(kcs []kamalConfig) bool {
	for _, k := range kcs {
		if k.Base != kcs[0].Base {
			return false
		}
	}
	return true
}

func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func filtersCover(filters []string, ctx string) bool {
	for _, f := range filters {
		f = strings.TrimPrefix(f, "./")
		if strings.HasPrefix(f, ctx+"/") || f == ctx {
			return true
		}
	}
	return false
}

func executable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func ReadUpstateMapping(root, service, destination string) (UpstateMapping, bool) {
	if destination == "" {
		destination = "default"
	}
	for _, name := range []string{service + "." + destination + ".json", destination + ".json", "config.json"} {
		raw, err := readFile(filepath.Join(root, ".upstate", name))
		if err != nil {
			continue
		}
		var m UpstateMapping
		if json.Unmarshal(raw, &m) != nil || m.EnvironmentID == "" {
			continue
		}
		if m.Service != "" && m.Service != service {
			continue
		}
		if m.Destination != "" && m.Destination != destination {
			continue
		}
		return m, true
	}
	return UpstateMapping{}, false
}
