package selection

import (
	"errors"
	"strings"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
)

var (
	ciWorkflow   = config.Workflow{Name: "CI", Path: ".github/workflows/ci.yml"}
	edgeWorkflow = config.Workflow{Name: "Phoenix edge", Path: ".github/workflows/edge.yml"}
)

func baseUnit(id, env string) config.Unit {
	return config.Unit{ID: id, Environment: env, Command: "bin/kamal", Config: "config/deploy.yml", Requires: []string{"CI"}, Lock: "host-a",
		Paths: []string{"**"}, SecretsFile: ".kamal/secrets", RegistryPassword: config.RegistryGitHubToken, Kamal: config.KamalBundler,
		SSH: config.SSH{User: "root", Port: 22, Hosts: []string{"203.0.113.10"}}}
}

func simpleConfig(t *testing.T) *config.Config {
	c := &config.Config{Version: 1, Branch: "main", ToolVersion: "v0.1.0", CI: []config.Workflow{ciWorkflow}, Unclassified: config.UnclassifiedDeploy,
		Ignore: []string{"docs/**", "**/*.md"}, Units: []config.Unit{baseUnit("web", "production")}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

// twoConfig models two services: Rails (root context) and Edge (services/edge) as standalone
// Kamal configs with their own wrappers on one shared host and kamal-proxy. Rails owns
// migrations; Edge deploys after Rails; schema changes deploy both.
func twoConfig(t *testing.T) *config.Config {
	rails := baseUnit("rails", "production-rails")
	rails.Command, rails.Config = "bin/kamal-rails", "config/deploy.rails.yml"
	rails.Paths = []string{"**", "!services/edge/**", "!config/deploy.edge.yml", "!bin/kamal-edge"}
	edge := baseUnit("edge", "production-edge")
	edge.Command, edge.Config = "bin/kamal-edge", "config/deploy.edge.yml"
	edge.Paths = []string{"services/edge/**", "config/deploy.edge.yml", "bin/kamal-edge"}
	edge.Requires = []string{"CI", "Phoenix edge"}
	edge.After = []string{"rails"}
	c := &config.Config{Version: 1, Branch: "main", ToolVersion: "v0.1.0", CI: []config.Workflow{ciWorkflow, edgeWorkflow}, Unclassified: config.UnclassifiedDeploy,
		Ignore: []string{"docs/**", "**/*.md"},
		Shared: []config.Shared{{Paths: []string{"db/schema.rb", "db/migrate/**", "bin/kamal", ".kamal/hooks/**"}, Units: []string{"rails", "edge"}}},
		Units:  []config.Unit{rails, edge}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

type world struct {
	t    *testing.T
	cfg  *config.Config
	repo *fakeRepo
	gh   *fakeGitHub
	api  GuardAPI
}

func newWorld(t *testing.T, cfg *config.Config) *world {
	repo := newFakeRepo()
	gh, client := newFakeGitHub(t, repo)
	return &world{t: t, cfg: cfg, repo: repo, gh: gh, api: client}
}

// run simulates one kamal-autodeploy workflow run: select, then for each unit in order
// the guard, Kamal (outcome), and record. Units whose predecessor failed are skipped, like
// the generated job `needs`.
func (w *world) run(event, trigger string, outcomes map[string]string, dispatch ...string) map[string]Decision {
	w.t.Helper()
	in := Input{Config: w.cfg, Repo: testRepo, Event: event, TriggerSHA: trigger, BranchRef: "refs/remotes/origin/main"}
	if len(dispatch) > 0 {
		in.Action, in.Unit = dispatch[0], dispatch[1]
		if len(dispatch) > 2 {
			in.Version = dispatch[2]
		}
	}
	decisions, err := Select(in, w.api, w.repo)
	if err != nil {
		w.t.Fatalf("select: %v", err)
	}
	out := map[string]Decision{}
	failed := map[string]bool{}
	order, _ := w.cfg.Order()
	for _, id := range order {
		var d Decision
		for _, x := range decisions {
			if x.Unit == id {
				d = x
			}
		}
		out[id] = d
		if d.Action == ActionSkip {
			continue
		}
		blocked := false
		for _, p := range w.cfg.Predecessors(id) {
			if failed[p] {
				blocked = true
			}
		}
		if blocked {
			failed[id] = true
			d.Action, d.Reason = ActionSkip, "predecessor failed"
			out[id] = d
			continue
		}
		unit, _ := w.cfg.Unit(id)
		g, err := Guard(w.api, w.cfg, unit, d, "1", "https://example.test/run/1")
		if errors.Is(err, ErrPreviousAutomaticFailure) {
			failed[id] = true // a nonzero guard exit fails the job and blocks dependants
			d.Action, d.Reason = ActionSkip, g.Reason
			out[id] = d
			continue
		}
		if err != nil {
			w.t.Fatalf("guard %s: %v", id, err)
		}
		if !g.Proceed {
			d.Action, d.Reason = ActionSkip, g.Reason
			out[id] = d
			continue
		}
		if g.DeploymentID == 0 {
			continue // doctor
		}
		outcome := outcomes[id]
		if outcome == "" {
			outcome = "success"
		}
		if outcome != "success" {
			failed[id] = true
		}
		if err := Record(w.api, g.DeploymentID, outcome, ""); err != nil {
			w.t.Fatalf("record %s: %v", id, err)
		}
	}
	return out
}

func (w *world) auto(trigger string, outcomes map[string]string) map[string]Decision {
	return w.run("workflow_run", trigger, outcomes)
}

func expect(t *testing.T, got map[string]Decision, want map[string]string) {
	t.Helper()
	for unit, action := range want {
		if got[unit].Action != action {
			t.Errorf("unit %s: action %q (reason %q), want %q", unit, got[unit].Action, got[unit].Reason, action)
		}
	}
}

func (w *world) baseline(unit string) string {
	u, _ := w.cfg.Unit(unit)
	st, err := ReadState(w.api, u)
	if err != nil {
		w.t.Fatal(err)
	}
	return st.Baseline
}

// seedDeployed puts a unit at sha with a successful recorded deploy.
func (w *world) seedDeployed(unit, sha string) {
	u, _ := w.cfg.Unit(unit)
	w.gh.recorded(u.Environment, unit, ActionDeploy, sha, "", "in_progress", "success")
}

func TestFirstDeploySingleConfig(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/models/user.rb": "1", "config/deploy.yml": "1"})
	w.repo.push(c1)
	w.gh.green(c1, ciWorkflow)
	got := w.auto(c1, nil)
	expect(t, got, map[string]string{"web": ActionDeploy})
	if !strings.Contains(got["web"].Reason, "first deploy") {
		t.Errorf("reason %q", got["web"].Reason)
	}
	if w.baseline("web") != c1 {
		t.Error("baseline did not advance to the deployed commit")
	}
}

func TestSimpleCodeChangeDeploysDocsOnlySkips(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	c2 := w.repo.commit("c2", c1, map[string]string{"docs/guide.md": "x", "README.md": "y"})
	c3 := w.repo.commit("c3", c2, map[string]string{"app/a.rb": "2"})
	w.repo.push(c1, c2, c3)
	w.seedDeployed("web", c1)
	w.gh.green(c2, ciWorkflow)
	expect(t, w.auto(c2, nil), map[string]string{"web": ActionSkip})
	w.gh.green(c3, ciWorkflow)
	got := w.auto(c3, nil)
	expect(t, got, map[string]string{"web": ActionDeploy})
	if got["web"].SHA != c3 || got["web"].Baseline != c1 {
		t.Errorf("deployed %s from baseline %s", got["web"].SHA, got["web"].Baseline)
	}
}

func TestDestinationOverlaysAreSeparateUnits(t *testing.T) {
	cfg := simpleConfig(t)
	staging := baseUnit("staging", "staging")
	staging.Destination, staging.Args = "staging", []string{"-d", "staging"}
	cfg.Units[0].Destination, cfg.Units[0].Args = "production", []string{"-d", "production"}
	cfg.Units = append(cfg.Units, staging)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, cfg)
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	c2 := w.repo.commit("c2", c1, map[string]string{"app/a.rb": "2"})
	w.repo.push(c1, c2)
	w.seedDeployed("web", c1)
	w.seedDeployed("staging", c2) // staging was deployed by hand dispatch already
	w.gh.green(c2, ciWorkflow)
	got := w.auto(c2, nil)
	expect(t, got, map[string]string{"web": ActionDeploy, "staging": ActionSkip})
	if cfg.Predecessors("staging")[0] != "web" {
		t.Error("units sharing a lock must be chained")
	}
}

func twoServiceHistory(w *world) (base string) {
	base = w.repo.commit("base", "", map[string]string{"app/models/a.rb": "1", "services/edge/lib/e.ex": "1", "db/schema.rb": "1", "config/deploy.rails.yml": "1", "config/deploy.edge.yml": "1"})
	w.repo.push(base)
	w.seedDeployed("rails", base)
	w.seedDeployed("edge", base)
	return base
}

func TestRailsOnlyEdgeOnlyAndDocsOnly(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)

	rails := w.repo.commit("rails", base, map[string]string{"app/models/a.rb": "2"})
	w.repo.push(rails)
	w.gh.green(rails, ciWorkflow) // Phoenix edge is path-filtered and does not run
	expect(t, w.auto(rails, nil), map[string]string{"rails": ActionDeploy, "edge": ActionSkip})

	edge := w.repo.commit("edge", rails, map[string]string{"services/edge/lib/e.ex": "2"})
	w.repo.push(edge)
	w.gh.green(edge, ciWorkflow, edgeWorkflow)
	expect(t, w.auto(edge, nil), map[string]string{"rails": ActionSkip, "edge": ActionDeploy})

	docs := w.repo.commit("docs", edge, map[string]string{"docs/plan.md": "x"})
	w.repo.push(docs)
	w.gh.green(docs, ciWorkflow)
	got := w.auto(docs, nil)
	expect(t, got, map[string]string{"rails": ActionSkip, "edge": ActionSkip})
	if got["edge"].Attention != "" || got["rails"].Attention != "" {
		t.Errorf("docs-only change must not need attention: %+v", got)
	}
}

func TestMigrationDeploysBothInOrder(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	m := w.repo.commit("migration", base, map[string]string{"db/migrate/2026_add.rb": "1", "db/schema.rb": "2"})
	w.repo.push(m)
	w.gh.green(m, ciWorkflow, edgeWorkflow)
	got := w.auto(m, nil)
	expect(t, got, map[string]string{"rails": ActionDeploy, "edge": ActionDeploy})
	order, _ := w.cfg.Order()
	if order[0] != "rails" || order[1] != "edge" {
		t.Errorf("order %v: Rails owns the schema and must deploy first", order)
	}
	// Rails fails: Edge (after rails, same host) must not deploy, and keeps its baseline.
	m2 := w.repo.commit("migration2", m, map[string]string{"db/schema.rb": "3"})
	w.repo.push(m2)
	w.gh.green(m2, ciWorkflow, edgeWorkflow)
	got = w.auto(m2, map[string]string{"rails": "failure"})
	if got["edge"].Action != ActionSkip || w.baseline("edge") != m || w.baseline("rails") != m {
		t.Errorf("after Rails failure: edge %+v, baselines rails=%s edge=%s", got["edge"], short(w.baseline("rails")), short(w.baseline("edge")))
	}
}

func TestSharedInfraWrapperChangeDeploysBoth(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	c := w.repo.commit("hooks", base, map[string]string{".kamal/hooks/pre-deploy": "x"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow, edgeWorkflow)
	expect(t, w.auto(c, nil), map[string]string{"rails": ActionDeploy, "edge": ActionDeploy})
}

func TestUnclassifiedNeverSkips(t *testing.T) {
	cfg := twoConfig(t)
	// Narrow Rails so a root file belongs to nobody.
	cfg.Units[0].Paths = []string{"app/**", "config/**", "db/**"}
	w := newWorld(t, cfg)
	base := twoServiceHistory(w)
	c := w.repo.commit("unclassified", base, map[string]string{"lib/tasks/new.rake": "x"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow, edgeWorkflow)
	expect(t, w.auto(c, nil), map[string]string{"rails": ActionDeploy, "edge": ActionDeploy})

	cfg.Unclassified = config.UnclassifiedStop
	c2 := w.repo.commit("unclassified2", c, map[string]string{"lib/tasks/other.rake": "x"})
	w.repo.push(c2)
	w.gh.green(c2, ciWorkflow, edgeWorkflow)
	got := w.auto(c2, nil)
	expect(t, got, map[string]string{"rails": ActionSkip, "edge": ActionSkip})
	if got["rails"].Attention == "" {
		t.Error("unclassified stop must ask for attention, not skip silently")
	}
	// An owned file in the same diff must not let the unclaimed one through.
	c3 := w.repo.commit("mixed", c2, map[string]string{"app/models/a.rb": "9", "infra/new.tf": "x"})
	w.repo.push(c3)
	w.gh.green(c3, ciWorkflow, edgeWorkflow)
	got = w.auto(c3, nil)
	if got["rails"].Action != ActionSkip || got["rails"].Attention == "" {
		t.Errorf("mixed owned + unclassified under stop must stop: %+v", got["rails"])
	}
}

func TestCIFailureBlocks(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	c2 := w.repo.commit("c2", c1, map[string]string{"app/a.rb": "2"})
	w.repo.push(c1, c2)
	w.seedDeployed("web", c1)
	w.gh.ci(c2, ciWorkflow, "completed", "failure")
	got := w.auto(c2, nil)
	expect(t, got, map[string]string{"web": ActionSkip})
	if w.gh.count("web") != 1 {
		t.Error("a failed CI run must not record a deployment")
	}
	// Skipped and cancelled conclusions are not success either.
	for _, conclusion := range []string{"skipped", "cancelled", "neutral"} {
		c := w.repo.commit("c-"+conclusion, w.repo.head(), map[string]string{"app/a.rb": conclusion})
		w.repo.push(c)
		w.gh.ci(c, ciWorkflow, "completed", conclusion)
		expect(t, w.auto(c, nil), map[string]string{"web": ActionSkip})
	}
}

func TestFailedDeployThenDocsMergeStillDeploys(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	c2 := w.repo.commit("c2", c1, map[string]string{"app/a.rb": "2"})
	c3 := w.repo.commit("c3", c2, map[string]string{"docs/x.md": "docs"})
	w.repo.push(c1, c2)
	w.seedDeployed("web", c1)
	w.gh.green(c2, ciWorkflow)
	expect(t, w.auto(c2, map[string]string{"web": "failure"}), map[string]string{"web": ActionDeploy})
	if w.baseline("web") != c1 {
		t.Fatal("a failed deploy must not advance the baseline")
	}
	w.repo.push(c3)
	w.gh.green(c3, ciWorkflow)
	got := w.auto(c3, nil)
	expect(t, got, map[string]string{"web": ActionDeploy})
	if got["web"].Baseline != c1 || !strings.Contains(got["web"].Reason, "app/a.rb") {
		t.Errorf("docs merge after failure must still ship app/a.rb: %+v", got["web"])
	}
}

func TestPartialSuccessAdvancesOnlySucceededUnit(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	both := w.repo.commit("both", base, map[string]string{"app/models/a.rb": "2", "services/edge/lib/e.ex": "2"})
	w.repo.push(both)
	w.gh.green(both, ciWorkflow, edgeWorkflow)
	w.auto(both, map[string]string{"edge": "failure"})
	if w.baseline("rails") != both || w.baseline("edge") != base {
		t.Fatalf("partial success: rails=%s edge=%s", short(w.baseline("rails")), short(w.baseline("edge")))
	}
	docs := w.repo.commit("docs", both, map[string]string{"docs/x.md": "1"})
	w.repo.push(docs)
	w.gh.green(docs, ciWorkflow, edgeWorkflow)
	expect(t, w.auto(docs, nil), map[string]string{"rails": ActionSkip, "edge": ActionDeploy})
}

func TestTwoRapidMergesNeverDeployStale(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	s1 := w.repo.commit("s1", c1, map[string]string{"app/a.rb": "2"})
	s2 := w.repo.commit("s2", s1, map[string]string{"app/b.rb": "1"})
	w.repo.push(c1, s1, s2)
	w.seedDeployed("web", c1)
	// CI for s2 finishes first: deploy s2 (which contains s1).
	w.gh.green(s2, ciWorkflow)
	got := w.auto(s2, nil)
	if got["web"].SHA != s2 {
		t.Fatalf("deployed %s, want s2", short(got["web"].SHA))
	}
	// CI for s1 finishes later: its run must not roll production back to s1.
	w.gh.green(s1, ciWorkflow)
	expect(t, w.auto(s1, nil), map[string]string{"web": ActionSkip})
	if w.baseline("web") != s2 || w.gh.count("web") != 2 {
		t.Error("stale run recorded a deployment")
	}
}

func TestQueuedRunReplacementKeepsNewestGreenCommit(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	s1 := w.repo.commit("s1", c1, map[string]string{"app/a.rb": "2"})
	s2 := w.repo.commit("s2", s1, map[string]string{"app/b.rb": "1"})
	w.repo.push(c1, s1, s2)
	w.seedDeployed("web", c1)
	// The run for s2 was queued behind another run and replaced by the (older) s1 run.
	// The s1 run must still deploy s2, the newest green commit, so nothing is lost.
	w.gh.green(s1, ciWorkflow)
	w.gh.green(s2, ciWorkflow)
	got := w.auto(s1, nil)
	if got["web"].SHA != s2 {
		t.Fatalf("deployed %s, want newest green s2", short(got["web"].SHA))
	}
	// A newer commit still in CI is waited for, not deployed and not skipped as done.
	s3 := w.repo.commit("s3", s2, map[string]string{"app/c.rb": "1"})
	w.repo.push(s3)
	w.gh.ci(s3, ciWorkflow, "in_progress", "")
	got = w.auto(s2, nil)
	if got["web"].Action != ActionSkip || !strings.Contains(got["web"].Reason, "waiting") {
		t.Errorf("got %+v", got["web"])
	}
}

func TestStaleGuardSkipsWhenNewerDeployedMeanwhile(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	s1 := w.repo.commit("s1", c1, map[string]string{"app/a.rb": "2"})
	s2 := w.repo.commit("s2", s1, map[string]string{"app/a.rb": "3"})
	w.repo.push(c1, s1)
	w.seedDeployed("web", c1)
	w.gh.green(s1, ciWorkflow)
	decisions, err := Select(Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: s1, BranchRef: "refs/remotes/origin/main"}, w.api, w.repo)
	if err != nil || decisions[0].Action != ActionDeploy {
		t.Fatalf("%v %+v", err, decisions)
	}
	// Before this run's deploy job starts, another run deploys s2.
	w.repo.push(s2)
	w.seedDeployed("web", s2)
	unit, _ := w.cfg.Unit("web")
	g, err := Guard(w.api, w.cfg, unit, decisions[0], "1", "")
	if err != nil || g.Proceed || !strings.Contains(g.Reason, "stale") {
		t.Fatalf("guard must refuse a stale deploy: %+v %v", g, err)
	}
}

func TestForcePushRemovedCommitIsStale(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	s1 := w.repo.commit("s1", c1, map[string]string{"app/a.rb": "2"})
	w.repo.push(c1, s1)
	w.seedDeployed("web", c1)
	w.gh.green(s1, ciWorkflow)
	decisions, _ := Select(Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: s1, BranchRef: "refs/remotes/origin/main"}, w.api, w.repo)
	// main is force-pushed to a different commit before the deploy job runs.
	other := w.repo.commit("rewritten", c1, map[string]string{"app/a.rb": "9"})
	w.repo.branch = []string{c1, other}
	unit, _ := w.cfg.Unit("web")
	g, err := Guard(w.api, w.cfg, unit, decisions[0], "1", "")
	if err != nil || g.Proceed {
		t.Fatalf("guard must refuse a commit no longer on main: %+v %v", g, err)
	}
}

func TestForcePushedBaselineDeploysConservatively(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	old := w.repo.commit("old", "", map[string]string{"app/a.rb": "1"})
	w.repo.missing[old] = true // garbage collected after a force-push
	c1 := w.repo.commit("c1", "", map[string]string{"docs/a.md": "1"})
	w.repo.push(c1)
	w.seedDeployed("web", old)
	w.gh.green(c1, ciWorkflow)
	got := w.auto(c1, nil)
	expect(t, got, map[string]string{"web": ActionDeploy})
	if !strings.Contains(got["web"].Reason, "baseline unknown") {
		t.Errorf("reason %q", got["web"].Reason)
	}
}

func TestRenamesAndDeletesCountForBothSides(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	// Moving a file from Edge into Rails touches both units.
	mv := w.repo.commit("mv", base, map[string]string{"services/edge/lib/e.ex": "", "app/models/e.rb": "1"})
	w.repo.push(mv)
	w.gh.green(mv, ciWorkflow, edgeWorkflow)
	expect(t, w.auto(mv, nil), map[string]string{"rails": ActionDeploy, "edge": ActionDeploy})
}

func TestRequiredWorkflowPendingWaitsAndAbsentNeedsAttention(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	e := w.repo.commit("edge", base, map[string]string{"services/edge/lib/e.ex": "2"})
	w.repo.push(e)
	w.gh.green(e, ciWorkflow)
	w.gh.ci(e, edgeWorkflow, "in_progress", "")
	got := w.auto(e, nil)
	if got["edge"].Action != ActionSkip || !strings.Contains(got["edge"].Reason, "waiting") {
		t.Errorf("pending Phoenix edge must wait: %+v", got["edge"])
	}
	// Phoenix edge completes: its run deploys Edge.
	w.gh.green(e, edgeWorkflow)
	expect(t, w.auto(e, nil), map[string]string{"edge": ActionDeploy})
	// A unit change whose required workflow never ran is not deployed and is flagged.
	e2 := w.repo.commit("edge2", e, map[string]string{"config/deploy.edge.yml": "2"})
	w.repo.push(e2)
	w.gh.green(e2, ciWorkflow)
	got = w.auto(e2, nil)
	if got["edge"].Action != ActionSkip || got["edge"].Attention == "" {
		t.Errorf("absent required workflow must not count as success: %+v", got["edge"])
	}
}

func TestUntrustedRunsAreIgnored(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c1)
	// A fork PR run with a branch named main, a pull_request run, and another branch.
	fork := github_run(c1, "pull_request", "main", "evil/shop")
	pr := github_run(c1, "pull_request", "main", testRepo)
	other := github_run(c1, "push", "feature", testRepo)
	for _, r := range []struct{ event, branch, repo string }{{fork.Event, fork.HeadBranch, fork.HeadRepository.FullName}, {pr.Event, pr.HeadBranch, pr.HeadRepository.FullName}, {other.Event, other.HeadBranch, other.HeadRepository.FullName}} {
		run := github_run(c1, r.event, r.branch, r.repo)
		w.gh.addRun(run)
	}
	forkPush := github_run(c1, "push", "main", "evil/shop")
	w.gh.addRun(forkPush)
	got := w.auto(c1, nil)
	if got["web"].Action != ActionSkip {
		t.Errorf("untrusted runs must never count as CI success: %+v", got["web"])
	}
	// A commit that is not on main is refused outright.
	stray := w.repo.commit("stray", c1, map[string]string{"app/a.rb": "x"})
	if _, err := Select(Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: stray, BranchRef: "refs/remotes/origin/main"}, w.api, w.repo); err == nil {
		t.Error("a commit not on main must be refused")
	}
}

func TestWorkflowDispatchDeployRollbackAndHold(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	c := w.repo.commit("c", base, map[string]string{"docs/a.md": "1"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow, edgeWorkflow)

	got := w.run("workflow_dispatch", c, nil, ActionDeploy, "edge")
	expect(t, got, map[string]string{"edge": ActionDeploy, "rails": ActionSkip})
	if w.baseline("edge") != c {
		t.Error("manual deploy must record the deployed commit")
	}

	got = w.run("workflow_dispatch", c, nil, ActionRollback, "rails", base)
	expect(t, got, map[string]string{"rails": ActionRollback})
	d := w.gh.latest("rails")
	if d.payload.Kind != ActionRollback || d.payload.Version != base {
		t.Errorf("rollback recorded as %+v", d.payload)
	}
	// After a rollback, automatic deploys hold the unit instead of redeploying the bad code.
	c2 := w.repo.commit("c2", c, map[string]string{"app/models/a.rb": "3"})
	w.repo.push(c2)
	w.gh.green(c2, ciWorkflow, edgeWorkflow)
	got = w.auto(c2, nil)
	if got["rails"].Action != ActionSkip || !strings.Contains(got["rails"].Reason, "held") {
		t.Errorf("rolled-back unit must be held: %+v", got["rails"])
	}
	// A manual deploy resumes it.
	expect(t, w.run("workflow_dispatch", c2, nil, ActionDeploy, "rails"), map[string]string{"rails": ActionDeploy})
	c3 := w.repo.commit("c3", c2, map[string]string{"app/models/a.rb": "4"})
	w.repo.push(c3)
	w.gh.green(c3, ciWorkflow, edgeWorkflow)
	expect(t, w.auto(c3, nil), map[string]string{"rails": ActionDeploy})

	// Invalid rollback versions are refused before anything runs.
	got = w.run("workflow_dispatch", c3, nil, ActionRollback, "rails", "$(id)")
	if got["rails"].Action != ActionSkip || got["rails"].Attention == "" {
		t.Errorf("invalid version must be refused: %+v", got["rails"])
	}
	// Doctor runs never record a deployment.
	before := w.gh.count("edge")
	expect(t, w.run("workflow_dispatch", c3, nil, ActionDoctor, "edge"), map[string]string{"edge": ActionDoctor})
	if w.gh.count("edge") != before {
		t.Error("doctor must not create a deployment")
	}
}

func TestManualDeployRefusesWithoutGreenCI(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c1)
	w.gh.ci(c1, ciWorkflow, "completed", "failure")
	got := w.run("workflow_dispatch", c1, nil, ActionDeploy, "web")
	if got["web"].Action != ActionSkip || got["web"].Attention == "" {
		t.Errorf("got %+v", got["web"])
	}
}

func TestRecoveryAfterFailedRollback(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	c2 := w.repo.commit("c2", c1, map[string]string{"app/a.rb": "2"})
	w.repo.push(c1, c2)
	w.seedDeployed("web", c2)
	w.gh.green(c2, ciWorkflow)
	w.run("workflow_dispatch", c2, map[string]string{"web": "failure"}, ActionRollback, "web", c1)
	c3 := w.repo.commit("c3", c2, map[string]string{"app/a.rb": "3"})
	w.repo.push(c3)
	w.gh.green(c3, ciWorkflow)
	got := w.auto(c3, nil)
	if got["web"].Action != ActionSkip || got["web"].Attention == "" {
		t.Fatalf("a failed rollback must stop automatic deploys and ask for attention: %+v", got["web"])
	}
	// A failed recovery must not clear the hold.
	w.run("workflow_dispatch", c3, map[string]string{"web": "failure"}, ActionDeploy, "web")
	if got := w.auto(c3, nil); got["web"].Attention == "" {
		t.Fatal("failed recovery cleared failed rollback hold")
	}
	// Recovery: a successful manual deploy (or rollback) clears it.
	expect(t, w.run("workflow_dispatch", c3, nil, ActionDeploy, "web"), map[string]string{"web": ActionDeploy})
	c4 := w.repo.commit("c4", c3, map[string]string{"app/a.rb": "4"})
	w.repo.push(c4)
	w.gh.green(c4, ciWorkflow)
	expect(t, w.auto(c4, nil), map[string]string{"web": ActionDeploy})
}

func TestGitHubAPIErrorsStopInsteadOfGuessing(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c1)
	w.gh.green(c1, ciWorkflow)
	in := Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: c1, BranchRef: "refs/remotes/origin/main"}
	for _, path := range []string{"/repos/" + testRepo + "/deployments", "/repos/" + testRepo + "/actions/runs"} {
		w.gh.failPaths = map[string]int{path: 502}
		if _, err := Select(in, w.api, w.repo); err == nil {
			t.Errorf("API failure on %s must stop selection", path)
		}
	}
	w.gh.failPaths = map[string]int{}
}

func TestPaginationFindsOlderBaseline(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	w.gh.pageSize = 2
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	c2 := w.repo.commit("c2", c1, map[string]string{"docs/a.md": "1"})
	w.repo.push(c1, c2)
	w.seedDeployed("web", c1)
	for i := 0; i < 5; i++ { // five failed attempts after the last success
		w.gh.recorded("production", "web", ActionDeploy, c2, "", "in_progress", "failure")
	}
	if w.baseline("web") != c1 {
		t.Fatal("pagination must reach the older successful deploy")
	}
	// A history longer than the walk cap is unknown: deploy, never skip.
	w.gh.pageSize = 1
	for i := 0; i < 12; i++ {
		w.gh.recorded("production", "web", ActionDeploy, c2, "", "failure")
	}
	w.gh.green(c2, ciWorkflow)
	got := w.auto(c2, nil)
	if got["web"].Action != ActionDeploy || !strings.Contains(got["web"].Reason, "unknown") {
		t.Errorf("got %+v", got["web"])
	}
}

func TestDeploymentsOfOtherToolsAreIgnored(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c1)
	w.gh.mu.Lock()
	d := &fakeDeployment{states: []string{"success"}}
	d.ID, d.SHA, d.Task, d.Environment = 1, c1, "deploy", "production" // GitHub's own environment deployment
	w.gh.deployments = append(w.gh.deployments, d)
	w.gh.mu.Unlock()
	w.gh.green(c1, ciWorkflow)
	expect(t, w.auto(c1, nil), map[string]string{"web": ActionDeploy})
}

func TestInactiveStatusAfterSuccessKeepsBaseline(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c1 := w.repo.commit("c1", "", map[string]string{"app/a.rb": "1"})
	c2 := w.repo.commit("c2", c1, map[string]string{"docs/a.md": "1"})
	w.repo.push(c1, c2)
	w.gh.recorded("production", "web", ActionDeploy, c1, "", "in_progress", "success", "inactive")
	w.gh.green(c2, ciWorkflow)
	expect(t, w.auto(c2, nil), map[string]string{"web": ActionSkip})
}

func TestManualDispatchReconcilesOtherUnits(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	c := w.repo.commit("pending-rails", base, map[string]string{"app/models/a.rb": "new"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow, edgeWorkflow)
	got := w.run("workflow_dispatch", c, nil, ActionDeploy, "edge")
	expect(t, got, map[string]string{"edge": ActionDeploy, "rails": ActionDeploy})
}

func TestRollbackVersionMatchesShellValidation(t *testing.T) {
	for _, bad := range []string{"_release", ".release", "-release", "", "v 1"} {
		if ValidVersion(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, good := range []string{"v1.2_3-4", "abc123", "1"} {
		if !ValidVersion(good) {
			t.Errorf("refused %q", good)
		}
	}
}

func TestFirstDeployDocsOnlyAbsentCIUsesChosenCommit(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	c1 := w.repo.commit("first-green", "", map[string]string{"services/edge/lib/e.ex": "1"})
	docs := w.repo.commit("docs", c1, map[string]string{"docs/a.md": "1"})
	w.repo.push(c1, docs)
	w.gh.green(c1, ciWorkflow, edgeWorkflow)
	w.gh.green(docs, ciWorkflow)
	got := w.auto(docs, nil)
	if got["edge"].Action != ActionDeploy || got["edge"].SHA != c1 || got["edge"].Attention != "" {
		t.Fatalf("unexpected first deploy: %+v", got["edge"])
	}
}

func TestDuplicateFailedAutomaticAttemptAndManualRetry(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c := w.repo.commit("first", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow)
	expect(t, w.auto(c, map[string]string{"web": "failure"}), map[string]string{"web": ActionDeploy})
	before := w.gh.count("web")
	got := w.auto(c, nil)
	expect(t, got, map[string]string{"web": ActionSkip})
	if !strings.Contains(got["web"].Reason, "already failed") || w.gh.count("web") != before {
		t.Fatalf("duplicate must skip without a new record: %+v", got)
	}
	expect(t, w.run("workflow_dispatch", c, nil, ActionDeploy, "web"), map[string]string{"web": ActionDeploy})
	if w.baseline("web") != c {
		t.Fatal("manual retry did not recover")
	}
}

func TestFailedAttemptRetriesOnNewHeadWithSameEligibleVersion(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	c := w.repo.commit("edge-change", base, map[string]string{"services/edge/lib/e.ex": "new"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow, edgeWorkflow)
	w.auto(c, map[string]string{"edge": "failure"})
	expect(t, w.auto(c, nil), map[string]string{"rails": ActionSkip, "edge": ActionSkip})
	docs := w.repo.commit("docs-recovery", c, map[string]string{"docs/x.md": "recovery"})
	w.repo.push(docs)
	w.gh.green(docs, ciWorkflow) // Edge CI deliberately does not run for docs.
	got := w.auto(docs, nil)
	expect(t, got, map[string]string{"rails": ActionSkip, "edge": ActionDeploy})
	if got["edge"].SHA != c {
		t.Fatalf("expected original eligible version, got %+v", got["edge"])
	}
}

func TestGuardSuppressesFailureRecordedAfterSelection(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	c := w.repo.commit("first", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow)
	decisions, err := Select(Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: c, BranchRef: "refs/remotes/origin/main"}, w.api, w.repo)
	if err != nil {
		t.Fatal(err)
	}
	w.auto(c, map[string]string{"web": "failure"})
	before := w.gh.count("web")
	g, err := Guard(w.api, w.cfg, w.cfg.Units[0], decisions[0], "queued", "")
	if !errors.Is(err, ErrPreviousAutomaticFailure) {
		t.Fatalf("suppression must fail the job: %v", err)
	}
	if g.Proceed || !strings.Contains(g.Reason, "already failed") || w.gh.count("web") != before {
		t.Fatalf("queued decision bypassed suppression: %+v", g)
	}
}

func TestSuppressedPredecessorFailureStillBlocksDependent(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	base := twoServiceHistory(w)
	c := w.repo.commit("shared-schema", base, map[string]string{"db/schema.rb": "migration"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow, edgeWorkflow)
	edgeBefore := w.gh.count("edge")
	first := w.auto(c, map[string]string{"rails": "failure"})
	expect(t, first, map[string]string{"rails": ActionDeploy, "edge": ActionSkip})
	railsAfter := w.gh.count("rails")
	second := w.auto(c, nil)
	expect(t, second, map[string]string{"rails": ActionSkip, "edge": ActionSkip})
	if !strings.Contains(second["rails"].Reason, "already failed") || second["edge"].Reason != "predecessor failed" {
		t.Fatalf("failure did not propagate: %+v", second)
	}
	if w.gh.count("edge") != edgeBefore || w.gh.count("rails") != railsAfter {
		t.Fatal("suppressed run created another deployment")
	}
	if w.baseline("rails") != base || w.baseline("edge") != base {
		t.Fatal("failed migration advanced a baseline")
	}
}

func TestManualActivationDisablesCITriggersButAllowsExplicitDeploy(t *testing.T) {
	cfg := simpleConfig(t)
	cfg.Activation = config.ActivationManual
	cfg.ToolVersion = "v0.1.4"
	w := newWorld(t, cfg)
	c := w.repo.commit("initial", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow)
	before := w.gh.count("web")
	got := w.auto(c, nil)
	expect(t, got, map[string]string{"web": ActionSkip})
	if w.gh.count("web") != before {
		t.Fatal("manual activation created automatic deployment")
	}
	// Even a stale/forged automatic decision must not open a deployment under this workflow.
	_, err := Guard(w.api, cfg, cfg.Units[0], Decision{Unit: "web", Action: ActionDeploy, SHA: c, AutomaticHead: c}, "run", "")
	if err == nil || w.gh.count("web") != before {
		t.Fatal("guard allowed automatic work in manual activation")
	}
	expect(t, w.run("workflow_dispatch", c, nil, ActionDoctor, "web"), map[string]string{"web": ActionDoctor})
	if w.gh.count("web") != before {
		t.Fatal("doctor created a deployment")
	}
	expect(t, w.run("workflow_dispatch", c, nil, ActionDeploy, "web"), map[string]string{"web": ActionDeploy})
}

func TestManualActivationStillRequiresGreenCI(t *testing.T) {
	cfg := simpleConfig(t)
	cfg.Activation = config.ActivationManual
	cfg.ToolVersion = "v0.1.4"
	w := newWorld(t, cfg)
	c := w.repo.commit("initial", "", map[string]string{"app/a.rb": "1"})
	w.repo.push(c)
	w.gh.ci(c, ciWorkflow, "completed", "failure")
	expect(t, w.run("workflow_dispatch", c, nil, ActionDeploy, "web"), map[string]string{"web": ActionSkip})
	if w.gh.count("web") != 0 {
		t.Fatal("manual dispatch bypassed required CI")
	}
}

func TestManualActivationReconcilesOrderedPrerequisitesOnDispatch(t *testing.T) {
	cfg := twoConfig(t)
	cfg.Activation = config.ActivationManual
	cfg.ToolVersion = "v0.1.4"
	w := newWorld(t, cfg)
	base := twoServiceHistory(w)
	c := w.repo.commit("schema", base, map[string]string{"db/schema.rb": "new"})
	w.repo.push(c)
	w.gh.green(c, ciWorkflow, edgeWorkflow)
	got := w.run("workflow_dispatch", c, nil, ActionDeploy, "edge")
	expect(t, got, map[string]string{"rails": ActionDeploy, "edge": ActionDeploy})
}

func TestPendingCIReasonMatchesActivation(t *testing.T) {
	for _, mode := range []string{config.ActivationManual, config.ActivationAutomatic} {
		t.Run(mode, func(t *testing.T) {
			cfg := twoConfig(t)
			cfg.Activation = mode
			cfg.ToolVersion = "v0.1.4"
			w := newWorld(t, cfg)
			base := twoServiceHistory(w)
			c := w.repo.commit("rails-pending", base, map[string]string{"app/a.rb": "pending"})
			w.repo.push(c)
			w.gh.ci(c, ciWorkflow, "in_progress", "")
			got := w.run("workflow_dispatch", c, nil, ActionDeploy, "edge")
			reason := got["rails"].Reason
			if mode == config.ActivationManual {
				if !strings.Contains(reason, "dispatch again after CI passes") || strings.Contains(reason, "completion starts another run") {
					t.Fatalf("manual pending guidance: %s", reason)
				}
			} else if !strings.Contains(reason, "completion starts another run") {
				t.Fatalf("automatic guidance changed: %s", reason)
			}
		})
	}
}
