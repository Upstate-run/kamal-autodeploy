package selection

import (
	"errors"
	"fmt"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/github"
)

// ErrPreviousAutomaticFailure keeps dependent jobs blocked when a failed attempt is suppressed.
var ErrPreviousAutomaticFailure = errors.New("previous automatic deployment attempt failed")

// GuardAPI is what the deploy job's last check before Kamal needs.
type GuardAPI interface {
	API
	Compare(base, head string) (string, error)
	BranchHead(branch string) (string, error)
	CreateDeployment(sha, environment, task string, payload github.DeploymentPayload, description string) (int64, error)
	CreateStatus(id int64, state, logURL, description string) error
}

// GuardResult says whether the deploy job may run Kamal, and the Deployment it opened.
type GuardResult struct {
	Proceed      bool
	Reason       string
	DeploymentID int64
}

// Guard runs inside the deploy job after the concurrency lock is held, right before Kamal.
// A deploy is stale (and skipped) when another run has meanwhile deployed this commit or a
// newer one, or when the commit is no longer on the branch. Rollbacks and doctor runs are
// explicit requests and are never skipped as stale. For deploy and rollback it opens a
// Deployment with status in_progress; Record closes it.
func Guard(api GuardAPI, cfg *config.Config, unit config.Unit, d Decision, runID, logURL string) (GuardResult, error) {
	if d.AutomaticHead != "" && !cfg.Automatic() {
		return GuardResult{}, errors.New("automatic deployment is disabled (activation: manual)")
	}
	if d.Action == ActionDoctor {
		return GuardResult{Proceed: true, Reason: "connectivity check"}, nil
	}
	if d.Action != ActionDeploy && d.Action != ActionRollback {
		return GuardResult{Reason: "nothing to do"}, nil
	}
	if d.Action == ActionDeploy {
		st, err := ReadState(api, unit)
		if err != nil {
			return GuardResult{}, err
		}
		if st.Baseline != "" && st.Baseline != d.Baseline {
			rel, err := api.Compare(d.SHA, st.Baseline)
			if err != nil {
				return GuardResult{}, err
			}
			if rel == "ahead" || rel == "identical" {
				return GuardResult{Reason: fmt.Sprintf("stale: %s was deployed meanwhile and already contains %s", short(st.Baseline), short(d.SHA))}, nil
			}
		}
		if d.AutomaticHead != "" {
			failed, err := failedAutomaticAttempt(api, unit, d)
			if err != nil {
				return GuardResult{}, err
			}
			if failed {
				reason := fmt.Sprintf("automatic attempt for %s already failed at branch head %s; dispatch action=deploy to retry, or push a new commit", short(d.SHA), short(d.AutomaticHead))
				return GuardResult{Reason: reason}, fmt.Errorf("%w: %s", ErrPreviousAutomaticFailure, reason)
			}
		}
		head, err := api.BranchHead(cfg.Branch)
		if err != nil {
			return GuardResult{}, err
		}
		rel, err := api.Compare(d.SHA, head)
		if err != nil {
			return GuardResult{}, err
		}
		if rel != "ahead" && rel != "identical" {
			return GuardResult{Reason: fmt.Sprintf("stale: %s is no longer on %s (history was rewritten)", short(d.SHA), cfg.Branch)}, nil
		}
	}
	payload := github.DeploymentPayload{Unit: unit.ID, Kind: d.Action, Version: d.Version, RunID: runID, AutomaticHead: d.AutomaticHead}
	desc := "kamal-autodeploy " + d.Action + " " + unit.ID
	id, err := api.CreateDeployment(d.SHA, unit.Environment, config.DeploymentTask, payload, desc)
	if err != nil {
		return GuardResult{}, err
	}
	if err := api.CreateStatus(id, "in_progress", logURL, desc); err != nil {
		return GuardResult{}, err
	}
	return GuardResult{Proceed: true, Reason: d.Reason, DeploymentID: id}, nil
}

// Record closes the Deployment Guard opened. outcome is the deploy step's outcome
// (success, failure, cancelled, skipped). Only success advances the unit's baseline.
func Record(api GuardAPI, id int64, outcome, logURL string) error {
	state := "failure"
	switch outcome {
	case "success":
		state = "success"
	case "cancelled", "skipped":
		state = "error"
	}
	return api.CreateStatus(id, state, logURL, "kamal-autodeploy: "+outcome)
}

// Inspect durable attempt records under the deployment lock. A later branch head
// permits recovery even when path-filtered CI still selects the same older SHA.
// Legacy records and explicit manual attempts do not suppress automatic work.
func failedAutomaticAttempt(api API, unit config.Unit, decision Decision) (bool, error) {
	failed := false
	err := api.WalkDeployments(unit.Environment, config.DeploymentTask, func(d github.Deployment) error {
		if d.Payload.Unit != unit.ID || d.Payload.Kind != ActionDeploy || d.SHA != decision.SHA || d.Payload.AutomaticHead != decision.AutomaticHead {
			return nil
		}
		states, err := api.States(d.ID)
		if err != nil {
			return err
		}
		for _, state := range states {
			if state == "success" {
				return github.ErrStop
			}
		}
		for _, state := range states {
			if state == "failure" || state == "error" {
				failed = true
				return github.ErrStop
			}
		}
		return nil
	})
	// Preserve the existing unknown-history recovery policy when no matching attempt
	// can be found within the bounded history walk.
	if errors.Is(err, github.ErrTruncated) {
		return false, nil
	}
	return failed, err
}
