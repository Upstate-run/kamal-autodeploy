package selection

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/github"
)

const testRepo = "acme/shop"

// ---- fake git ------------------------------------------------------------------

type fakeRepo struct {
	parents  map[string][]string
	trees    map[string]map[string]string
	branch   []string // first-parent chain of the branch, oldest first
	messages map[string]string
	missing  map[string]bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{messages: map[string]string{}, parents: map[string][]string{}, trees: map[string]map[string]string{}, missing: map[string]bool{}}
}

func shaOf(s string) string { h := sha1.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

// commit adds a commit on top of parent with the given file edits ("" content deletes).
func (r *fakeRepo) commit(label, parent string, edits map[string]string) string {
	sha := shaOf(label)
	r.messages[sha] = label
	tree := map[string]string{}
	if parent != "" {
		for k, v := range r.trees[parent] {
			tree[k] = v
		}
		r.parents[sha] = []string{parent}
	}
	for k, v := range edits {
		if v == "" {
			delete(tree, k)
		} else {
			tree[k] = v
		}
	}
	r.trees[sha] = tree
	return sha
}

// push appends commits to the branch.
func (r *fakeRepo) push(shas ...string) { r.branch = append(r.branch, shas...) }

func (r *fakeRepo) head() string { return r.branch[len(r.branch)-1] }

func (r *fakeRepo) HasCommit(sha string) bool { _, ok := r.trees[sha]; return ok && !r.missing[sha] }

func (r *fakeRepo) IsAncestor(a, b string) (bool, error) {
	if !r.HasCommit(a) || !r.HasCommit(b) {
		return false, fmt.Errorf("unknown commit")
	}
	stack := []string{b}
	seen := map[string]bool{}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c == a {
			return true, nil
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		stack = append(stack, r.parents[c]...)
	}
	return false, nil
}

func (r *fakeRepo) ChangedFiles(from, to string) ([]string, error) {
	if !r.HasCommit(from) || !r.HasCommit(to) {
		return nil, fmt.Errorf("unknown commit")
	}
	a, b := r.trees[from], r.trees[to]
	out := []string{}
	for k, v := range a {
		if b[k] != v {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *fakeRepo) FirstParents(ref string, n int) ([]string, error) {
	out := []string{}
	for i := len(r.branch) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, r.branch[i])
	}
	return out, nil
}

func (r *fakeRepo) CommitMessage(sha string) (string, error) { return r.messages[sha], nil }

func (r *fakeRepo) Head() (string, error) { return r.head(), nil }

// ---- fake GitHub ---------------------------------------------------------------

type fakeDeployment struct {
	github.Deployment
	payload github.DeploymentPayload
	states  []string // oldest first
}

type fakeGitHub struct {
	mu          sync.Mutex
	t           *testing.T
	repo        *fakeRepo
	runs        []github.WorkflowRun
	deployments []*fakeDeployment // oldest first
	nextID      int64
	failPaths   map[string]int // path prefix -> HTTP status
	pageSize    int
	calls       int
}

func newFakeGitHub(t *testing.T, repo *fakeRepo) (*fakeGitHub, *github.Client) {
	f := &fakeGitHub{t: t, repo: repo, failPaths: map[string]int{}, pageSize: 100, nextID: 1000}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := github.New(srv.URL, "test-token", testRepo)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

// ci records a completed (or pending) push run of a workflow for sha.
func (f *fakeGitHub) ci(sha string, w config.Workflow, status, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	r := github.WorkflowRun{ID: f.nextID, Name: w.Name, Path: w.Path, HeadSHA: sha, HeadBranch: "main", Event: "push", Status: status, Conclusion: conclusion, CreatedAt: fmt.Sprintf("2026-11-04T10:%02d:00Z", f.nextID%60)}
	r.Repository.FullName, r.HeadRepository.FullName = testRepo, testRepo
	f.runs = append(f.runs, r)
}

func (f *fakeGitHub) addRun(r github.WorkflowRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, r)
}

func (f *fakeGitHub) green(sha string, ws ...config.Workflow) {
	for _, w := range ws {
		f.ci(sha, w, "completed", "success")
	}
}

// recorded seeds a deployment that this tool would have recorded.
func (f *fakeGitHub) recorded(env, unit, kind, sha, version string, states ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	d := &fakeDeployment{payload: github.DeploymentPayload{Unit: unit, Kind: kind, Version: version}, states: states}
	d.ID, d.SHA, d.Ref, d.Task, d.Environment = f.nextID, sha, sha, config.DeploymentTask, env
	f.deployments = append(f.deployments, d)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(401)
		return
	}
	for prefix, status := range f.failPaths {
		if strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(status)
			return
		}
	}
	base := "/repos/" + testRepo
	p := strings.TrimPrefix(r.URL.Path, base)
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && p == "/actions/runs":
		out := []github.WorkflowRun{}
		for _, run := range f.runs {
			if run.HeadSHA == q.Get("head_sha") && run.Event == q.Get("event") {
				out = append(out, run)
			}
		}
		f.page(w, r, len(out), func(lo, hi int) any {
			return map[string]any{"total_count": len(out), "workflow_runs": out[lo:hi]}
		})
	case r.Method == "GET" && p == "/deployments":
		out := []map[string]any{}
		for i := len(f.deployments) - 1; i >= 0; i-- {
			d := f.deployments[i]
			if d.Environment == q.Get("environment") && d.Task == q.Get("task") {
				out = append(out, map[string]any{"id": d.ID, "sha": d.SHA, "ref": d.Ref, "task": d.Task, "environment": d.Environment, "payload": d.payload})
			}
		}
		f.page(w, r, len(out), func(lo, hi int) any { return out[lo:hi] })
	case r.Method == "GET" && strings.HasPrefix(p, "/deployments/") && strings.HasSuffix(p, "/statuses"):
		d := f.find(p)
		if d == nil {
			w.WriteHeader(404)
			return
		}
		out := []map[string]string{}
		for i := len(d.states) - 1; i >= 0; i-- {
			out = append(out, map[string]string{"state": d.states[i]})
		}
		f.page(w, r, len(out), func(lo, hi int) any { return out[lo:hi] })
	case r.Method == "POST" && p == "/deployments":
		var body struct {
			Ref              string                   `json:"ref"`
			Environment      string                   `json:"environment"`
			Task             string                   `json:"task"`
			Payload          github.DeploymentPayload `json:"payload"`
			RequiredContexts []string                 `json:"required_contexts"`
			AutoMerge        bool                     `json:"auto_merge"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.RequiredContexts == nil || body.AutoMerge {
			f.t.Errorf("deployment must set required_contexts [] and auto_merge false")
		}
		f.nextID++
		d := &fakeDeployment{payload: body.Payload}
		d.ID, d.SHA, d.Ref, d.Task, d.Environment = f.nextID, body.Ref, body.Ref, body.Task, body.Environment
		f.deployments = append(f.deployments, d)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": d.ID})
	case r.Method == "POST" && strings.HasSuffix(p, "/statuses"):
		d := f.find(p)
		if d == nil {
			w.WriteHeader(404)
			return
		}
		var body struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.states = append(d.states, body.State)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == "GET" && strings.HasPrefix(p, "/compare/"):
		spec, _ := url.PathUnescape(strings.TrimPrefix(p, "/compare/"))
		baseSHA, headSHA, _ := strings.Cut(spec, "...")
		status := "diverged"
		switch {
		case baseSHA == headSHA:
			status = "identical"
		case must(f.repo.IsAncestor(baseSHA, headSHA)):
			status = "ahead"
		case must(f.repo.IsAncestor(headSHA, baseSHA)):
			status = "behind"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
	case r.Method == "GET" && strings.HasPrefix(p, "/commits/") && p != "/commits/main":
		sha := strings.TrimPrefix(p, "/commits/")
		parents := []map[string]string{}
		for _, parent := range f.repo.parents[sha] {
			parents = append(parents, map[string]string{"sha": parent})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "commit": map[string]string{"message": f.repo.messages[sha]}, "parents": parents})
	case r.Method == "GET" && p == "/commits/main":
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": f.repo.head()})
	default:
		f.t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}

func must(ok bool, err error) bool { return err == nil && ok }

func (f *fakeGitHub) find(p string) *fakeDeployment {
	parts := strings.Split(p, "/")
	if len(parts) < 3 {
		return nil
	}
	id, _ := strconv.ParseInt(parts[2], 10, 64)
	for _, d := range f.deployments {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// page serves a slice of n items with GitHub-style Link pagination.
func (f *fakeGitHub) page(w http.ResponseWriter, r *http.Request, n int, slice func(lo, hi int) any) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	lo := (page - 1) * f.pageSize
	if lo > n {
		lo = n
	}
	hi := lo + f.pageSize
	if hi > n {
		hi = n
	}
	if hi < n {
		u := *r.URL
		q := u.Query()
		q.Set("page", strconv.Itoa(page+1))
		u.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, r.Host, u.String()))
	}
	_ = json.NewEncoder(w).Encode(slice(lo, hi))
}

// latest is the newest recorded deployment for a unit.
func (f *fakeGitHub) latest(unit string) *fakeDeployment {
	for i := len(f.deployments) - 1; i >= 0; i-- {
		if f.deployments[i].payload.Unit == unit {
			return f.deployments[i]
		}
	}
	return nil
}

func (f *fakeGitHub) count(unit string) int {
	n := 0
	for _, d := range f.deployments {
		if d.payload.Unit == unit {
			n++
		}
	}
	return n
}

func github_run(sha, event, branch, headRepo string) github.WorkflowRun {
	r := github.WorkflowRun{ID: int64(len(event) + len(branch)*7 + len(headRepo)*31), Name: ciWorkflow.Name, Path: ciWorkflow.Path, HeadSHA: sha, HeadBranch: branch, Event: event, Status: "completed", Conclusion: "success", CreatedAt: "2026-11-04T11:00:00Z"}
	r.Repository.FullName, r.HeadRepository.FullName = testRepo, headRepo
	return r
}
