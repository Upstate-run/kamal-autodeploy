package cli

import (
	"errors"
	"fmt"
	"slices"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/discover"
)

// verifyHostsCommand reads the selected tree without evaluating ERB or running project
// scripts. Match the configured command and selected config/destination, and refuse
// unresolved targets or changed wrapper routing.
func verifyHostsCommand(e *Env, args []string) error {
	f := newFlags("verify-hosts")
	configPath := f.String("config", "", "")
	command := f.String("command", "", "")
	requireTailscale := f.Bool("require-tailscale", false, "")
	destination := f.String("destination", "", "")
	if err := f.Parse(args); err != nil {
		return errors.New("usage: kamal-autodeploy verify-hosts")
	}
	root, err := repoRoot(e.Dir)
	if err != nil {
		return err
	}
	report, err := discover.Discover(root, discover.Options{})
	if err != nil {
		return err
	}
	if report.Config == nil || len(report.Config.Units) == 0 {
		return errors.New("selected checkout has no discoverable targets")
	}
	found := false
	for _, unit := range report.Config.Units {
		if unit.Config != *configPath || unit.Destination != *destination {
			continue
		}
		if *command == "kamal" {
			// Direct invocation never executes a project wrapper. Validate its
			// complete routing against the chosen static config/destination.
			if !config.MatchDirectArgs(*configPath, *destination, f.Args()) {
				return errors.New("direct Kamal arguments conflict with the selected config/destination")
			}
		} else {
			if unit.Command != *command {
				continue
			}
			if !slices.Equal(unit.Args, f.Args()) {
				return errors.New("effective Kamal arguments differ from the discovered config/destination routing; refusing deployment")
			}
			route, ok := discover.CommandRouting(root, *command)
			if !ok || (route != "" && route != *configPath) {
				return errors.New("wrapper executable routing is unsupported or conflicts with the selected config")
			}
		}
		found = true
		for _, info := range report.Units {
			if info.ID == unit.ID && (info.HostsUnknown || info.EndpointUnknown) {
				return errors.New("selected checkout has unresolved SSH targets; refusing deployment")
			}
		}
		if len(unit.SSH.Hosts) == 0 {
			return errors.New("selected checkout has no static SSH targets")
		}
		for _, host := range unit.SSH.Hosts {
			if *requireTailscale && !config.TailscaleHost(host) {
				return fmt.Errorf("selected checkout target %s is not a direct tailnet peer; refusing public SSH fallback", host)
			}
			if !knownHostsCovers(e.Getenv("KNOWN_HOSTS"), host, unit.SSH.Port) {
				return fmt.Errorf("selected checkout target %s is not pinned; refusing deployment", host)
			}
		}
	}
	if !found {
		return errors.New("selected checkout config/destination is not statically discoverable; refusing deployment")
	}
	return nil
}
