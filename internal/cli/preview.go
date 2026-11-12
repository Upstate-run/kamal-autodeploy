package cli

import (
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
	"github.com/Upstate-run/kamal-autodeploy/internal/selection"
)

// previewCommand uses configuration from the PR base and reads Git objects only.
// It needs no API token, deployment state, project scripts or secret providers.
func previewCommand(e *Env, args []string) error {
	f := newFlags("preview")
	base := f.String("base", "", "PR base commit")
	head := f.String("head", "", "PR head commit")
	if err := f.Parse(args); err != nil || f.NArg() != 0 || !gitx.ValidSHA(*base) || !gitx.ValidSHA(*head) {
		return errors.New("usage: kamal-autodeploy preview --base SHA --head SHA")
	}
	root, err := repoRoot(e.Dir)
	if err != nil {
		return err
	}
	repo := gitx.Git{Dir: root}
	raw, err := repo.FileAt(*base, config.FileName)
	if err != nil {
		return errors.New("PR base has no readable " + config.FileName + "; initialize KAD on the base branch first")
	}
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		return err
	}
	ancestor, err := repo.MergeBase(*base, *head)
	if err != nil {
		return err
	}
	files, err := repo.ChangedFiles(ancestor, *head)
	if err != nil {
		return err
	}
	var summary strings.Builder
	summary.WriteString("### KAD PR deploy preview\n\nPlan only. No deploy has run.\n\n")
	summary.WriteString("Uses the base branch's KAD rules and this PR's changes. Actual deployment selection uses each unit's last successful deployment and required CI on the merged commit.\n\n")
	if selection.SkipDeploy(e.Getenv("PR_TITLE")) || selection.SkipDeploy(e.Getenv("PR_BODY")) {
		summary.WriteString("This PR requests [skip-deploy]. Automatic deployment will be held if the marker is preserved in the landed commit message.\n\n")
	}
	summary.WriteString("<table><tr><th>Unit</th><th>PR impact</th><th>Reason</th></tr>\n")
	for _, unit := range cfg.Units {
		cls := selection.Classify(cfg, unit.ID, files)
		impact, reason := "Unchanged", "No changes owned by this unit"
		switch {
		case len(cls.Unclassified) > 0 && cfg.Unclassified == config.UnclassifiedStop:
			impact, reason = "Review required", "Unclassified files stop automatic selection"
		case len(cls.Owned) > 0:
			impact, reason = "Would deploy after CI", fmt.Sprintf("%d owned or shared file(s)", len(cls.Owned))
		case len(cls.Unclassified) > 0:
			impact, reason = "Would deploy after CI", "Unclassified files use the deploy policy"
		}
		fmt.Fprintf(&summary, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(unit.ID), html.EscapeString(impact), html.EscapeString(reason))
	}
	summary.WriteString("</table>\n")
	review := []string{}
	for _, file := range files {
		needsReview := file == config.FileName || file == config.WorkflowPath
		for _, shared := range cfg.Shared {
			needsReview = needsReview || config.Match(shared.Paths, file)
		}
		for _, unit := range cfg.Units {
			needsReview = needsReview || file == unit.Config
		}
		if needsReview {
			review = append(review, file)
		}
	}
	if len(review) > 0 {
		summary.WriteString("\nShared or deployment configuration changes need review. Proposed rule changes take effect after merge.\n\n<ul>\n")
		for i, file := range review {
			if i == 100 {
				fmt.Fprintf(&summary, "<li>%d additional paths</li>\n", len(review)-i)
				break
			}
			fmt.Fprintf(&summary, "<li><code>%s</code></li>\n", html.EscapeString(file))
		}
		summary.WriteString("</ul>\n")
	}
	return e.appendFile("GITHUB_STEP_SUMMARY", summary.String())
}
