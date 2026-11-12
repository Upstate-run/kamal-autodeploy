// Package github is the small slice of the GitHub REST API the workflow needs: workflow
// runs for a commit, the per-unit Deployments that record each unit's deployed baseline,
// and commit comparison. Every list is paginated and capped; a cap or an API error is
// reported as an error so callers say "unknown" instead of guessing.
package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MaxPages caps every paginated list (100 items per page).
const MaxPages = 10

type Client struct {
	BaseURL string // https://api.github.com
	Token   string
	Repo    string // owner/name
	HTTP    *http.Client
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func New(baseURL, token, repo string) (*Client, error) {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if !repoPattern.MatchString(repo) {
		return nil, errors.New("GITHUB_REPOSITORY must be owner/name")
	}
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is required")
	}
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token, Repo: repo, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

// APIError is a non-2xx answer. The body is not included: it may echo request data.
type APIError struct {
	Status int
	Path   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub API %s answered %d", e.Path, e.Status)
}

func (c *Client) do(method, path string, body any, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	target := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		target = c.BaseURL + path
	} else if !strings.HasPrefix(path, c.BaseURL+"/") {
		return nil, errors.New("refusing to follow a pagination link to another host")
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "kamal-autodeploy")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API %s unreachable", redactPath(path))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, &APIError{Status: res.StatusCode, Path: redactPath(path)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("GitHub API %s returned unexpected JSON", redactPath(path))
		}
	}
	return res.Header, nil
}

func redactPath(p string) string {
	if u, err := url.Parse(p); err == nil {
		return u.Path
	}
	return "request"
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// pages fetches every page of a list (each page decoded by decode), up to MaxPages.
// Reaching the cap with a next page still pending is an error: the list is incomplete.
func (c *Client) pages(path string, decode func([]byte) (int, error)) error {
	next := path
	for page := 0; next != ""; page++ {
		if page == MaxPages {
			return fmt.Errorf("GitHub API %s has more than %d pages: %w", redactPath(path), MaxPages, ErrTruncated)
		}
		var raw json.RawMessage
		h, err := c.do("GET", next, nil, &raw)
		if err != nil {
			return err
		}
		if _, err := decode(raw); err != nil {
			return err
		}
		next = ""
		if m := nextLink.FindStringSubmatch(h.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return nil
}

// WorkflowRun is the part of a run selection needs.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	RunAttempt int    `json:"run_attempt"`
	CreatedAt  string `json:"created_at"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

// RunsForSHA lists push-event workflow runs for one commit.
func (c *Client) RunsForSHA(sha string) ([]WorkflowRun, error) {
	out := []WorkflowRun{}
	q := url.Values{"head_sha": {sha}, "event": {"push"}, "per_page": {"100"}}
	err := c.pages("/repos/"+c.Repo+"/actions/runs?"+q.Encode(), func(raw []byte) (int, error) {
		var page struct {
			Runs []WorkflowRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, errors.New("GitHub API workflow runs returned unexpected JSON")
		}
		out = append(out, page.Runs...)
		return len(page.Runs), nil
	})
	return out, err
}

// DeploymentPayload is what this tool stores on each Deployment it creates.
type DeploymentPayload struct {
	AutomaticHead string `json:"automatic_head,omitempty"`
	Unit          string `json:"unit"`
	Kind          string `json:"kind"` // deploy | rollback
	Version       string `json:"version,omitempty"`
	RunID         string `json:"run_id,omitempty"`
}

type Deployment struct {
	ID          int64             `json:"id"`
	SHA         string            `json:"sha"`
	Ref         string            `json:"ref"`
	Task        string            `json:"task"`
	Environment string            `json:"environment"`
	CreatedAt   string            `json:"created_at"`
	Payload     DeploymentPayload `json:"-"`
	RawPayload  json.RawMessage   `json:"payload"`
	// States are the deployment's statuses, newest first.
	States []string `json:"-"`
}

// ErrStop ends a WalkDeployments early.
var ErrStop = errors.New("stop")

// ErrTruncated means a list had more than MaxPages pages, so the result is incomplete.
var ErrTruncated = errors.New("deployment history is longer than the walk cap")

// WalkDeployments visits one environment's deployments for task, newest first, page by
// page, until visit returns ErrStop. Payloads that are not ours decode to an empty payload.
func (c *Client) WalkDeployments(environment, task string, visit func(Deployment) error) error {
	q := url.Values{"environment": {environment}, "task": {task}, "per_page": {"100"}}
	err := c.pages("/repos/"+c.Repo+"/deployments?"+q.Encode(), func(raw []byte) (int, error) {
		var page []Deployment
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, errors.New("GitHub API deployments returned unexpected JSON")
		}
		for _, d := range page {
			_ = json.Unmarshal(d.RawPayload, &d.Payload)
			if err := visit(d); err != nil {
				return 0, err
			}
		}
		return len(page), nil
	})
	switch {
	case errors.Is(err, ErrStop):
		return nil
	}
	return err
}

// States lists one deployment's status states, newest first.
func (c *Client) States(id int64) ([]string, error) {
	states := []string{}
	err := c.pages(fmt.Sprintf("/repos/%s/deployments/%d/statuses?per_page=100", c.Repo, id), func(raw []byte) (int, error) {
		var page []struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, errors.New("GitHub API deployment statuses returned unexpected JSON")
		}
		for _, s := range page {
			states = append(states, s.State)
		}
		return len(page), nil
	})
	return states, err
}

// CreateDeployment records an attempt for one unit at one exact commit. required_contexts
// is empty because the workflow has already verified CI for that commit itself.
func (c *Client) CreateDeployment(sha, environment, task string, payload DeploymentPayload, description string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	body := map[string]any{
		"ref": sha, "environment": environment, "task": task, "payload": payload,
		"auto_merge": false, "required_contexts": []string{}, "description": description,
		"production_environment": true, "transient_environment": false,
	}
	if _, err := c.do("POST", "/repos/"+c.Repo+"/deployments", body, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, errors.New("GitHub did not create the deployment")
	}
	return out.ID, nil
}

// CreateStatus appends a status (in_progress, success, failure, error, inactive).
func (c *Client) CreateStatus(id int64, state, logURL, description string) error {
	body := map[string]any{"state": state, "description": description, "auto_inactive": false}
	if logURL != "" {
		body["log_url"] = logURL
	}
	_, err := c.do("POST", fmt.Sprintf("/repos/%s/deployments/%d/statuses", c.Repo, id), body, nil)
	return err
}

// Compare returns GitHub's relation of head to base: ahead, behind, identical or diverged.
func (c *Client) Compare(base, head string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	if _, err := c.do("GET", fmt.Sprintf("/repos/%s/compare/%s...%s?per_page=1", c.Repo, url.PathEscape(base), url.PathEscape(head)), nil, &out); err != nil {
		return "", err
	}
	switch out.Status {
	case "ahead", "behind", "identical", "diverged":
		return out.Status, nil
	}
	return "", errors.New("GitHub compare returned an unknown status")
}

// BranchHead is the current commit of a branch.
func (c *Client) BranchHead(branch string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do("GET", "/repos/"+c.Repo+"/commits/"+url.PathEscape(branch), nil, &out); err != nil {
		return "", err
	}
	if len(out.SHA) != 40 {
		return "", errors.New("GitHub returned an invalid branch head")
	}
	return out.SHA, nil
}

// CommitMetadata supplies the immutable first-parent history for the deployment guard.
type CommitMetadata struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

func (c *Client) Commit(sha string) (CommitMetadata, error) {
	var commit CommitMetadata
	if !commitPattern.MatchString(sha) {
		return commit, errors.New("invalid commit")
	}
	_, err := c.do("GET", "/repos/"+c.Repo+"/commits/"+sha, nil, &commit)
	if err == nil && commit.SHA != sha {
		err = errors.New("GitHub returned a different commit")
	}
	if err == nil {
		for _, parent := range commit.Parents {
			if !commitPattern.MatchString(parent.SHA) {
				return commit, errors.New("GitHub returned an invalid commit parent")
			}
		}
	}
	return commit, err
}
