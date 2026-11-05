// Package selection decides, for one trusted commit on the deploy branch, which units to
// deploy. Each unit's baseline is its last successful deploy recorded as a GitHub
// Deployment in that unit's environment. Changes are computed from that baseline (not
// from HEAD^ or the triggering push), so a failed or skipped deploy never loses changes.
//
// The rules, in order, for an automatic (workflow_run) run:
//
//  1. A rollback that did not succeed is the newest attempt: stop, needs a person.
//  2. The newest successful deploy is a rollback: the unit is held until a manual deploy.
//  3. Pick the newest commit on the branch's first-parent chain (at most MaxCandidates,
//     always including the trigger) that is newer than the baseline and whose required
//     CI workflows all succeeded. Pending, failed, skipped or absent CI is never success.
//  4. No baseline, an unreadable history or a baseline commit missing from the
//     repository: deploy (never skip on unknown).
//  5. Otherwise classify the files changed between baseline and that commit. Owned or
//     shared files deploy the unit; unclassified files deploy it (or stop, by policy);
//     only ignored files and other units' files skip it.
package selection

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/github"
	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
)

// MaxCandidates bounds how far back on the first-parent chain a run looks for a green commit.
const MaxCandidates = 25

// Actions a unit's deploy job can take.
const (
	ActionDeploy   = "deploy"
	ActionRollback = "rollback"
	ActionDoctor   = "doctor"
	ActionSkip     = "skip"
)

// API is the GitHub surface selection reads. *github.Client implements it.
type API interface {
	RunsForSHA(sha string) ([]github.WorkflowRun, error)
	WalkDeployments(environment, task string, visit func(github.Deployment) error) error
	States(id int64) ([]string, error)
}

type Input struct {
	Config *config.Config
	Repo   string // owner/name; runs from any other repository are ignored
	// Event is workflow_run or workflow_dispatch.
	Event string
	// TriggerSHA is the verified head SHA of the CI run (workflow_run) or the dispatch commit.
	TriggerSHA string
	// BranchRef is the local ref of the deploy branch, e.g. refs/remotes/origin/main.
	BranchRef string
	// Dispatch inputs.
	Action, Unit, Version string
}

type Decision struct {
	// AutomaticHead scopes failed-attempt suppression to one observed branch head.
	// Empty for explicit dispatches, which may always retry.
	AutomaticHead string `json:"automatic_head,omitempty"`
	Unit          string `json:"unit"`
	Action        string `json:"action"`
	SHA           string `json:"sha,omitempty"`
	Baseline      string `json:"baseline,omitempty"`
	Version       string `json:"version,omitempty"`
	Reason        string `json:"reason"`
	// Attention is set when a person must act; the workflow's report job fails the run.
	Attention string `json:"attention,omitempty"`
}

// State is a unit's deployed state as recorded in GitHub Deployments.
type State struct {
	Baseline        string
	BaselineKind    string
	BaselineVersion string
	// Unknown is true when the history was too long to find a successful deploy.
	Unknown bool
	// FailedRollback is true when a rollback has not been followed by a successful recovery.
	FailedRollback bool
}

// ReadState walks the unit's deployments newest first until it finds a successful one.
func ReadState(api API, unit config.Unit) (State, error) {
	var st State
	err := api.WalkDeployments(unit.Environment, config.DeploymentTask, func(d github.Deployment) error {
		if d.Payload.Unit != unit.ID || !gitx.ValidSHA(d.SHA) {
			return nil // not recorded by this tool for this unit
		}
		states, err := api.States(d.ID)
		if err != nil {
			return err
		}
		succeeded := false
		for _, s := range states {
			if s == "success" {
				succeeded = true
			}
		}
		if d.Payload.Kind == ActionRollback && !succeeded {
			st.FailedRollback = true
		}
		if succeeded {
			st.Baseline, st.BaselineKind, st.BaselineVersion = d.SHA, d.Payload.Kind, d.Payload.Version
			return github.ErrStop
		}
		return nil
	})
	if errors.Is(err, github.ErrTruncated) {
		return State{Unknown: true, FailedRollback: st.FailedRollback}, nil
	}
	return st, err
}

// CI verdicts for one commit.
const (
	CISuccess = "success"
	CIPending = "pending"
	CIFailed  = "failed"
	CIAbsent  = "absent"
)

type ciCache struct {
	api   API
	in    Input
	cache map[string][]github.WorkflowRun
}

func (c *ciCache) runs(sha string) ([]github.WorkflowRun, error) {
	if r, ok := c.cache[sha]; ok {
		return r, nil
	}
	r, err := c.api.RunsForSHA(sha)
	if err != nil {
		return nil, err
	}
	c.cache[sha] = r
	return r, nil
}

// verdict combines the unit's required workflows for sha. detail names the workflows
// behind a non-success verdict.
func (c *ciCache) verdict(sha string, unit config.Unit) (string, string, error) {
	runs, err := c.runs(sha)
	if err != nil {
		return "", "", err
	}
	byVerdict := map[string][]string{}
	for _, name := range unit.Requires {
		w, _ := c.in.Config.Workflow(name)
		v := workflowVerdict(runs, c.in, sha, w)
		byVerdict[v] = append(byVerdict[v], name)
	}
	for _, v := range []string{CIFailed, CIPending, CIAbsent} {
		if len(byVerdict[v]) > 0 {
			return v, strings.Join(byVerdict[v], ", "), nil
		}
	}
	return CISuccess, "", nil
}

// workflowVerdict is the newest trusted push run of one workflow for sha.
func workflowVerdict(runs []github.WorkflowRun, in Input, sha string, w config.Workflow) string {
	var newest *github.WorkflowRun
	for i := range runs {
		r := &runs[i]
		path, _, _ := strings.Cut(r.Path, "@")
		if r.HeadSHA != sha || r.Event != "push" || r.HeadBranch != in.Config.Branch || path != w.Path ||
			!strings.EqualFold(r.Repository.FullName, in.Repo) || !strings.EqualFold(r.HeadRepository.FullName, in.Repo) {
			continue
		}
		if newest == nil || r.CreatedAt > newest.CreatedAt || (r.CreatedAt == newest.CreatedAt && r.ID > newest.ID) {
			newest = r
		}
	}
	switch {
	case newest == nil:
		return CIAbsent
	case newest.Status != "completed":
		return CIPending
	case newest.Conclusion == "success":
		return CISuccess
	}
	return CIFailed // failure, cancelled, skipped, neutral, timed_out, action_required...
}

// Classified is how one unit sees a list of changed files.
type Classified struct {
	Owned        []string // the unit's own or shared paths
	Unclassified []string // no unit and no ignore rule claims them
	Ignored      []string
	Other        []string // only other units' paths
}

// Classify applies ignore first, then the unit's paths and shared rules.
func Classify(cfg *config.Config, unitID string, files []string) Classified {
	var c Classified
	unit, _ := cfg.Unit(unitID)
	for _, f := range files {
		if config.Match(cfg.Ignore, f) {
			c.Ignored = append(c.Ignored, f)
			continue
		}
		owned := config.Match(unit.Paths, f)
		claimed := owned
		for _, s := range cfg.Shared {
			if config.Match(s.Paths, f) {
				claimed = true
				for _, id := range s.Units {
					if id == unitID {
						owned = true
					}
				}
			}
		}
		for _, u := range cfg.Units {
			if config.Match(u.Paths, f) {
				claimed = true
			}
		}
		switch {
		case owned:
			c.Owned = append(c.Owned, f)
		case claimed:
			c.Other = append(c.Other, f)
		default:
			c.Unclassified = append(c.Unclassified, f)
		}
	}
	return c
}

// Select returns one decision per unit, in config order. An error means the run cannot
// decide safely (GitHub API failure, untrusted trigger); nothing should deploy.
func Select(in Input, api API, repo gitx.Repo) ([]Decision, error) {
	if in.Event == "workflow_dispatch" {
		if _, ok := in.Config.Unit(in.Unit); !ok {
			return nil, errors.New("unknown dispatch unit")
		}
		if in.Action != ActionDeploy && in.Action != ActionRollback && in.Action != ActionDoctor {
			return nil, errors.New("unknown dispatch action")
		}
	}
	if !gitx.ValidSHA(in.TriggerSHA) {
		return nil, errors.New("trigger SHA is not a full commit SHA")
	}
	candidates, err := repo.FirstParents(in.BranchRef, MaxCandidates)
	if err != nil || len(candidates) == 0 {
		return nil, errors.New("cannot read the deploy branch history")
	}
	head := candidates[0]
	onBranch, err := repo.IsAncestor(in.TriggerSHA, head)
	if err != nil || !onBranch {
		return nil, fmt.Errorf("commit %s is not on %s; refusing to deploy it", short(in.TriggerSHA), in.Config.Branch)
	}
	if !containsStr(candidates, in.TriggerSHA) {
		candidates = append(candidates, in.TriggerSHA)
	}
	ci := &ciCache{api: api, in: in, cache: map[string][]github.WorkflowRun{}}
	out := []Decision{}
	for _, unit := range in.Config.Units {
		var d Decision
		switch in.Event {
		case "workflow_run":
			d, err = automatic(in, unit, api, repo, ci, candidates)
		case "workflow_dispatch":
			if unit.ID != in.Unit && in.Action != ActionDoctor {
				d, err = automatic(in, unit, api, repo, ci, candidates)
			} else {
				d, err = dispatch(in, unit, api, ci, candidates, head)
			}
		default:
			return nil, errors.New("unsupported event; only workflow_run and workflow_dispatch deploy")
		}
		if err != nil {
			return nil, fmt.Errorf("unit %s: %w", unit.ID, err)
		}
		out = append(out, d)
	}
	return out, nil
}

func automatic(in Input, unit config.Unit, api API, repo gitx.Repo, ci *ciCache, candidates []string) (Decision, error) {
	d := Decision{Unit: unit.ID, Action: ActionSkip, AutomaticHead: candidates[0]}
	st, err := ReadState(api, unit)
	if err != nil {
		return d, err
	}
	d.Baseline = st.Baseline
	if st.FailedRollback {
		d.Reason = "the last rollback did not succeed, so the running version is unknown"
		d.Attention = fmt.Sprintf("%s: last rollback failed; dispatch kamal-autodeploy with action=deploy or action=rollback for this unit", unit.ID)
		return d, nil
	}
	if st.BaselineKind == ActionRollback {
		d.Reason = fmt.Sprintf("held at rollback version %s; automatic deploys resume after a manual deploy (action=deploy)", st.BaselineVersion)
		return d, nil
	}
	baselineKnown := st.Baseline != "" && !st.Unknown && repo.HasCommit(st.Baseline)
	chosen := ""
	var waiting, failed []string
	absent := ""
	absentDetail := ""
	for _, c := range candidates {
		if baselineKnown {
			if c == st.Baseline {
				break
			}
			older, err := repo.IsAncestor(c, st.Baseline)
			if err != nil {
				return d, err
			}
			if older {
				break
			}
		}
		v, detail, err := ci.verdict(c, unit)
		if err != nil {
			return d, err
		}
		if v == CISuccess {
			chosen = c
			break
		}
		switch v {
		case CIPending:
			waiting = append(waiting, short(c)+" ("+detail+")")
		case CIFailed:
			failed = append(failed, short(c)+" ("+detail+")")
		case CIAbsent:
			if absent == "" {
				absent, absentDetail = c, detail
			}
		}
	}
	// A newer commit whose required workflow never ran will not start another run for this
	// unit. If it changes the unit, say so instead of leaving the change silently undeployed.
	if absent != "" {
		from := chosen
		if from == "" {
			from = st.Baseline
		}
		relevant := true
		if from != "" && (chosen != "" || baselineKnown) {
			files, err := repo.ChangedFiles(from, absent)
			if err != nil {
				return d, err
			}
			cls := Classify(in.Config, unit.ID, files)
			relevant = len(cls.Owned)+len(cls.Unclassified) > 0
		}
		if relevant {
			d.Attention = fmt.Sprintf("%s: %s changes this unit but required workflow %s did not run for it; it was not deployed", unit.ID, short(absent), absentDetail)
		}
	}
	if chosen == "" {
		switch {
		case len(waiting) > 0:
			d.Reason = "waiting for CI on " + strings.Join(waiting, ", ") + "; its completion starts another run"
		case absent != "" && d.Attention != "":
			d.Reason = "required workflow " + absentDetail + " did not run for " + short(absent)
		case len(failed) > 0:
			d.Reason = "no newer commit passed CI (" + strings.Join(failed, ", ") + ")"
		case st.Baseline == "":
			d.Reason = "no commit with passing CI yet"
		default:
			d.Reason = "already deployed at " + short(st.Baseline) + " (or newer)"
		}
		return d, nil
	}
	d.SHA = chosen
	switch {
	case st.Unknown:
		d.Action, d.Reason = ActionDeploy, "deployment history too long to find the last successful deploy; deploying (baseline unknown)"
		return d, nil
	case st.Baseline == "":
		d.Action, d.Reason = ActionDeploy, "first deploy: no successful deploy recorded for this unit"
		return d, nil
	case !baselineKnown:
		d.Action, d.Reason = ActionDeploy, "deployed commit "+short(st.Baseline)+" is not in this repository's history (force-push?); deploying (baseline unknown)"
		return d, nil
	}
	files, err := repo.ChangedFiles(st.Baseline, chosen)
	if err != nil {
		return d, err
	}
	cls := Classify(in.Config, unit.ID, files)
	switch {
	case len(cls.Unclassified) > 0 && in.Config.Unclassified == config.UnclassifiedStop:
		// Checked first: owned files in the same diff must not slip unclaimed ones through.
		d.Reason = "unclassified change(s) and unclassified: stop: " + sample(cls.Unclassified)
		d.Attention = fmt.Sprintf("%s: changed files belong to no unit (%s); add them to a unit's paths, shared or ignore", unit.ID, sample(cls.Unclassified))
	case len(cls.Owned) > 0:
		d.Action, d.Reason = ActionDeploy, fmt.Sprintf("%d changed file(s) since %s: %s", len(cls.Owned), short(st.Baseline), sample(cls.Owned))
	case len(cls.Unclassified) > 0:
		d.Action, d.Reason = ActionDeploy, "unclassified change(s) deploy every unit: "+sample(cls.Unclassified)
	default:
		d.Reason = "no relevant changes since " + short(st.Baseline)
	}
	return d, nil
}

func dispatch(in Input, unit config.Unit, api API, ci *ciCache, candidates []string, head string) (Decision, error) {
	d := Decision{Unit: unit.ID, Action: ActionSkip, Reason: "not selected"}
	if in.Unit != unit.ID {
		return d, nil
	}
	switch in.Action {
	case ActionDoctor:
		d.Action, d.SHA, d.Reason = ActionDoctor, head, "connectivity check requested"
		return d, nil
	case ActionRollback:
		if !ValidVersion(in.Version) {
			d.Reason = "rollback needs a version (letters, digits, . _ -)"
			d.Attention = unit.ID + ": rollback refused: invalid version"
			return d, nil
		}
		st, err := ReadState(api, unit)
		if err != nil {
			return d, err
		}
		d.Action, d.SHA, d.Version, d.Baseline, d.Reason = ActionRollback, head, in.Version, st.Baseline, "manual rollback to "+in.Version
		return d, nil
	case ActionDeploy:
		st, err := ReadState(api, unit)
		if err != nil {
			return d, err
		}
		d.Baseline = st.Baseline
		var notes []string
		for _, c := range candidates {
			v, detail, err := ci.verdict(c, unit)
			if err != nil {
				return d, err
			}
			if v == CISuccess {
				d.Action, d.SHA = ActionDeploy, c
				d.Reason = "manual deploy of the newest commit with passing CI"
				if c != head {
					d.Reason += " (newer commits: " + strings.Join(notes, ", ") + ")"
				}
				return d, nil
			}
			notes = append(notes, short(c)+" "+v+" ("+detail+")")
		}
		d.Reason = "no recent commit on " + in.Config.Branch + " has passing CI"
		d.Attention = unit.ID + ": manual deploy refused: no recent commit has passing required CI"
		return d, nil
	}
	return d, errors.New("unknown dispatch action")
}

// ValidVersion is the rollback version shape (Kamal versions are usually full SHAs or tags).
func ValidVersion(v string) bool {
	if v == "" || len(v) > 128 || !(v[0] >= 'a' && v[0] <= 'z' || v[0] >= 'A' && v[0] <= 'Z' || v[0] >= '0' && v[0] <= '9') {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func sample(files []string) string {
	sorted := append([]string{}, files...)
	sort.Strings(sorted)
	if len(sorted) > 5 {
		return strings.Join(sorted[:5], ", ") + fmt.Sprintf(" and %d more", len(sorted)-5)
	}
	return strings.Join(sorted, ", ")
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
