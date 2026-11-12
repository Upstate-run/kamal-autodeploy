package selection

import (
	"fmt"
	"strings"
	"testing"
)

func TestSkipDeployToken(t *testing.T) {
	for _, message := range []string{"[skip-deploy]", "change [skip-deploy]", "change\n\n[skip-deploy]\n"} {
		if !SkipDeploy(message) {
			t.Errorf("missing marker in %q", message)
		}
	}
	for _, message := range []string{"[skip-ci]", "[SKIP-DEPLOY]", "prefix[skip-deploy]", "[skip-deploy]suffix", "`[skip-deploy]`"} {
		if SkipDeploy(message) {
			t.Errorf("false marker in %q", message)
		}
	}
}

func TestBatchHoldAndCumulativeRelease(t *testing.T) {
	w := newWorld(t, twoConfig(t))
	a := w.repo.commit("baseline", "", map[string]string{"app/a.rb": "old"})
	w.repo.push(a)
	w.gh.green(a, ciWorkflow, edgeWorkflow)
	w.run("workflow_run", a, nil)
	b := w.repo.commit("rails [skip-deploy]", a, map[string]string{"app/a.rb": "new"})
	c := w.repo.commit("edge [skip-deploy]", b, map[string]string{"services/edge/a.ex": "new"})
	for _, sha := range []string{b, c} {
		w.repo.push(sha)
		w.gh.green(sha, ciWorkflow, edgeWorkflow)
		ds := w.run("workflow_run", sha, nil)
		for _, d := range ds {
			if d.Action != ActionSkip || !strings.Contains(d.Reason, "[skip-deploy]") {
				t.Fatalf("hold: %+v", d)
			}
		}
	}
	d := w.repo.commit("release documentation", c, map[string]string{"docs/release.md": "release"})
	w.repo.push(d)
	// An earlier completion cannot release a newer batch while release CI is absent.
	w.run("workflow_run", b, nil)
	for _, unit := range []string{"rails", "edge"} {
		if w.gh.latest(unit).SHA != a {
			t.Fatal("hold advanced the baseline")
		}
	}
	w.gh.ci(d, ciWorkflow, "completed", "failure")
	w.run("workflow_run", d, nil)
	if w.gh.latest("rails").SHA != a {
		t.Fatal("failed release deployed")
	}
	w.gh.green(d, ciWorkflow)
	ds := w.run("workflow_run", d, nil)
	if ds["rails"].Action != ActionDeploy || ds["edge"].Action != ActionSkip {
		t.Fatalf("separate CI: %+v", ds)
	}
	w.gh.green(d, edgeWorkflow)
	w.run("workflow_run", d, nil)
	for _, unit := range []string{"rails", "edge"} {
		if w.gh.latest(unit).SHA != d || w.gh.count(unit) != 2 {
			t.Fatalf("%s release was not deployed once", unit)
		}
	}
}

func TestHoldOnFirstDeployAndManualOverride(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	a := w.repo.commit("[skip-deploy] first commit", "", map[string]string{"app/a.rb": "new"})
	w.repo.push(a)
	w.gh.green(a, ciWorkflow)
	if d := w.run("workflow_run", a, nil)["web"]; d.Action != ActionSkip {
		t.Fatal(d)
	}
	if w.gh.count("web") != 0 {
		t.Fatal("held first deploy created a deployment")
	}
	if d := w.run("workflow_dispatch", a, nil, ActionDeploy, "web")["web"]; d.Action != ActionDeploy {
		t.Fatal(d)
	}
}

func TestGuardBlocksNewHoldBeforeCreatingDeployment(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	a := w.repo.commit("selected", "", map[string]string{"app/a.rb": "new"})
	w.repo.push(a)
	w.gh.green(a, ciWorkflow)
	in := Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: a, BranchRef: "refs/remotes/origin/main"}
	ds, err := Select(in, w.api, w.repo)
	if err != nil {
		t.Fatal(err)
	}
	b := w.repo.commit("later [skip-deploy]", a, nil)
	w.repo.push(b)
	res, err := Guard(w.api, w.cfg, w.cfg.Units[0], ds[0], "1", "")
	if err != nil || !res.Held || res.Proceed || w.gh.count("web") != 0 {
		t.Fatalf("guard: %+v %v", res, err)
	}
}

func TestOldTriggerCannotCrossBatchOutsideCandidateWindow(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	a := w.repo.commit("old green", "", map[string]string{"app/a.rb": "old"})
	w.repo.push(a)
	w.gh.green(a, ciWorkflow)
	parent := a
	for i := 0; i <= MaxCandidates; i++ {
		parent = w.repo.commit(fmt.Sprintf("batch %d [skip-deploy]", i), parent, nil)
		w.repo.push(parent)
	}
	if d := w.run("workflow_run", a, nil)["web"]; d.Action != ActionSkip {
		t.Fatal(d)
	}
}

type unreadableMessageRepo struct{ *fakeRepo }

func (r unreadableMessageRepo) CommitMessage(string) (string, error) {
	return "", fmt.Errorf("commit metadata unavailable")
}

func TestUnreadableMarkerMetadataStopsSelection(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	a := w.repo.commit("first", "", nil)
	w.repo.push(a)
	w.gh.green(a, ciWorkflow)
	_, err := Select(Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: a, BranchRef: "refs/remotes/origin/main"}, w.api, unreadableMessageRepo{w.repo})
	if err == nil || w.gh.count("web") != 0 {
		t.Fatal("unreadable history allowed deployment")
	}
}

func TestGuardCommitLookupFailureCannotOpenDeployment(t *testing.T) {
	w := newWorld(t, simpleConfig(t))
	a := w.repo.commit("first", "", nil)
	w.repo.push(a)
	w.gh.green(a, ciWorkflow)
	ds, err := Select(Input{Config: w.cfg, Repo: testRepo, Event: "workflow_run", TriggerSHA: a, BranchRef: "refs/remotes/origin/main"}, w.api, w.repo)
	if err != nil {
		t.Fatal(err)
	}
	w.gh.failPaths["/repos/"+testRepo+"/commits/"+a] = 503
	res, err := Guard(w.api, w.cfg, w.cfg.Units[0], ds[0], "1", "")
	if err == nil || res.Proceed || w.gh.count("web") != 0 {
		t.Fatal("API failure allowed deployment")
	}
}
