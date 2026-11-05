package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/generate"
	"github.com/Upstate-run/kamal-autodeploy/internal/github"
	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
	"github.com/Upstate-run/kamal-autodeploy/internal/selection"
)

// These commands run inside the generated workflow. They read GITHUB_* variables that
// GitHub sets, write step outputs to $GITHUB_OUTPUT and a summary to $GITHUB_STEP_SUMMARY.

func ciClient(e *Env) (*github.Client, error) {
	return github.New(e.Getenv("GITHUB_API_URL"), e.Getenv("GITHUB_TOKEN"), e.Getenv("GITHUB_REPOSITORY"))
}

func loadRepoConfig(e *Env) (*config.Config, string, error) {
	root, err := repoRoot(e.Dir)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		return nil, "", err
	}
	return cfg, root, nil
}

// appendFile appends to a GitHub-provided file ($GITHUB_OUTPUT, $GITHUB_STEP_SUMMARY), or
// to the command's output when run outside Actions.
func (e *Env) appendFile(envName, text string) error {
	path := e.Getenv(envName)
	if path == "" {
		fmt.Fprint(e.Out, text)
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}

func selectCommand(e *Env, args []string) error {
	f := newFlags("select")
	event := f.String("event", "", "")
	sha := f.String("sha", "", "")
	action := f.String("action", "", "")
	unit := f.String("unit", "", "")
	version := f.String("version", "", "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return errors.New("usage: kamal-autodeploy select --event E --sha SHA [--action A --unit U --version V]")
	}
	cfg, root, err := loadRepoConfig(e)
	if err != nil {
		return err
	}
	// The workflow running now must be the generation of this config (or knowingly hand-edited).
	if wf, err := generate.Workflow(cfg); err == nil {
		existing, _ := os.ReadFile(filepath.Join(root, config.WorkflowPath))
		switch generate.Classify(existing, wf) {
		case generate.Outdated:
			return errors.New(config.WorkflowPath + " is out of date with " + config.FileName + "; run kamal-autodeploy init and commit the result")
		case generate.HandEdited, generate.Foreign:
			fmt.Fprintln(e.Err, "::notice::the deploy workflow was edited by hand; selection uses "+config.FileName)
		}
	}
	if *event == "workflow_dispatch" {
		if _, ok := cfg.Unit(*unit); !ok {
			return errors.New("unknown unit " + strconv.Quote(*unit))
		}
		switch *action {
		case selection.ActionDeploy, selection.ActionRollback, selection.ActionDoctor:
		default:
			return errors.New("action must be deploy, rollback or doctor")
		}
	}
	client, err := ciClient(e)
	if err != nil {
		return err
	}
	in := selection.Input{Config: cfg, Repo: client.Repo, Event: *event, TriggerSHA: *sha, BranchRef: "refs/remotes/origin/" + cfg.Branch,
		Action: *action, Unit: *unit, Version: *version}
	decisions, err := selection.Select(in, client, gitx.Git{Dir: root})
	if err != nil {
		return err
	}
	var out, summary strings.Builder
	var attention []string
	summary.WriteString("### kamal-autodeploy selection\n\n| Unit | Action | Commit | Baseline | Reason |\n| --- | --- | --- | --- | --- |\n")
	for _, d := range decisions {
		k := generate.OutputKey(d.Unit)
		fmt.Fprintf(&out, "%s_action=%s\n%s_sha=%s\n%s_baseline=%s\n%s_version=%s\n", k, d.Action, k, d.SHA, k, d.Baseline, k, d.Version)
		fmt.Fprintf(&out, "%s_automatic_head=%s\n", k, d.AutomaticHead)
		fmt.Fprintf(&summary, "| %s | %s | %s | %s | %s |\n", d.Unit, d.Action, shortSHA(d.SHA), shortSHA(d.Baseline), mdEscape(d.Reason))
		if d.Attention != "" {
			attention = append(attention, oneLine(d.Attention))
		}
		fmt.Fprintf(e.Err, "%s: %s (%s)\n", d.Unit, d.Action, oneLine(d.Reason))
	}
	fmt.Fprintf(&out, "attention=%s\n", strings.Join(attention, "; "))
	if len(attention) > 0 {
		summary.WriteString("\n**Needs attention:** " + mdEscape(strings.Join(attention, "; ")) + "\n")
	}
	if err := e.appendFile("GITHUB_OUTPUT", out.String()); err != nil {
		return err
	}
	return e.appendFile("GITHUB_STEP_SUMMARY", summary.String())
}

func shortSHA(s string) string {
	if len(s) >= 12 {
		return "`" + s[:12] + "`"
	}
	if s == "" {
		return "-"
	}
	return s
}

func mdEscape(s string) string { return strings.ReplaceAll(oneLine(s), "|", "\\|") }

// guardCommand needs no config: the deploy job checks out the selected application commit,
// whose .kamal-autodeploy.yml may be older than the one selection used. The workflow passes the
// unit's environment and the branch, which it embeds from the config it was generated from.
func guardCommand(e *Env, args []string) error {
	f := newFlags("guard")
	unitID := f.String("unit", "", "")
	environment := f.String("environment", "", "")
	branch := f.String("branch", "", "")
	action := f.String("action", "", "")
	sha := f.String("sha", "", "")
	baseline := f.String("baseline", "", "")
	automaticHead := f.String("automatic-head", "", "")
	version := f.String("version", "", "")
	runURL := f.String("run-url", "", "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return errors.New("usage: kamal-autodeploy guard --unit U --environment E --branch B --action A --sha SHA [--baseline SHA] [--automatic-head SHA] [--version V] [--run-url URL]")
	}
	if !config.ValidID(*unitID) || *environment == "" || *branch == "" {
		return errors.New("guard needs --unit, --environment and --branch")
	}
	if !gitx.ValidSHA(*sha) || (*baseline != "" && !gitx.ValidSHA(*baseline)) || (*automaticHead != "" && !gitx.ValidSHA(*automaticHead)) {
		return errors.New("invalid commit SHA")
	}
	if *action == selection.ActionRollback && !selection.ValidVersion(*version) {
		return errors.New("invalid rollback version")
	}
	client, err := ciClient(e)
	if err != nil {
		return err
	}
	unit := config.Unit{ID: *unitID, Environment: *environment}
	cfg := &config.Config{Branch: *branch}
	runID := e.Getenv("GITHUB_RUN_ID")
	d := selection.Decision{Unit: unit.ID, Action: *action, SHA: *sha, Baseline: *baseline, Version: *version, AutomaticHead: *automaticHead}
	res, err := selection.Guard(client, cfg, unit, d, runID, *runURL)
	if err != nil {
		return err
	}
	out := fmt.Sprintf("proceed=%t\n", res.Proceed)
	if res.DeploymentID != 0 {
		out += fmt.Sprintf("deployment_id=%d\n", res.DeploymentID)
	}
	fmt.Fprintf(e.Err, "%s: %s (%s)\n", unit.ID, map[bool]string{true: "proceeding", false: "not deploying"}[res.Proceed], oneLine(res.Reason))
	if !res.Proceed {
		_ = e.appendFile("GITHUB_STEP_SUMMARY", fmt.Sprintf("**%s:** not deployed: %s\n", unit.ID, mdEscape(res.Reason)))
	}
	return e.appendFile("GITHUB_OUTPUT", out)
}

func recordCommand(e *Env, args []string) error {
	f := newFlags("record")
	id := f.Int64("deployment-id", 0, "")
	outcome := f.String("outcome", "", "")
	runURL := f.String("run-url", "", "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 || *id <= 0 {
		return errors.New("usage: kamal-autodeploy record --deployment-id N --outcome success|failure|cancelled|skipped")
	}
	client, err := ciClient(e)
	if err != nil {
		return err
	}
	return selection.Record(client, *id, *outcome, *runURL)
}
