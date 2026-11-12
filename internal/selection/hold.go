package selection

import (
	"regexp"

	"github.com/Upstate-run/kamal-autodeploy/internal/gitx"
)

// Whitespace-delimited token prevents accidental matches inside prose or examples.
var skipDeployToken = regexp.MustCompile(`(^|[[:space:]])\[skip-deploy\]([[:space:]]|$)`)

// SkipDeploy reports the marker in a landed commit's subject or body.
func SkipDeploy(message string) bool {
	return skipDeployToken.MatchString(message)
}

func automaticCandidates(repo gitx.Repo, candidates []string) ([]string, string, error) {
	for i, sha := range candidates {
		message, err := repo.CommitMessage(sha)
		if err != nil {
			return nil, "", err
		}
		if SkipDeploy(message) {
			return candidates[:i], sha, nil
		}
	}
	return candidates, "", nil
}
