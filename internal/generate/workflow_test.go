package generate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"gopkg.in/yaml.v3"
)

func receiptInstallStep(t *testing.T) map[string]any {
	t.Helper()
	cfg, err := config.Load("../../examples/simple/.kamal-autodeploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Upstate = &config.Upstate{CLIVersion: "0.2.13", SHA256: strings.Repeat("a", 64)}
	raw, err := Workflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, j := range tree["jobs"].(map[string]any) {
		for _, s := range j.(map[string]any)["steps"].([]any) {
			step := s.(map[string]any)
			name, _ := step["name"].(string)
			if strings.HasPrefix(name, "Install upstate CLI ") {
				found = step
				if step["continue-on-error"] != true {
					t.Fatal("optional reporting blocks job")
				}
			} else if step["continue-on-error"] == true {
				t.Fatalf("mandatory step %s became optional", name)
			}
		}
	}
	if found == nil {
		t.Fatal("missing reporting install step")
	}
	return found
}

func TestOptionalReceiptInstallationVerifiesBeforePublishingPath(t *testing.T) {
	step := receiptInstallStep(t)
	for _, kind := range []string{"download", "checksum", "archive", "nonexecutable", "symlink", "success"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			runner := filepath.Join(dir, "runner")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(runner, 0700); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(dir, "fixture.tar.gz")
			f, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "archive" {
				_, err = f.WriteString("invalid gzip")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				gz := gzip.NewWriter(f)
				tw := tar.NewWriter(gz)
				data := []byte("#!/bin/sh\nexit 0\n")
				mode := int64(0755)
				if kind == "nonexecutable" {
					mode = 0644
				}
				h := &tar.Header{Name: "upstate", Mode: mode, Size: int64(len(data))}
				if kind == "symlink" {
					h.Typeflag = tar.TypeSymlink
					h.Linkname = "/bin/sh"
					h.Size = 0
				}
				if err := tw.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if kind != "symlink" {
					if _, err := tw.Write(data); err != nil {
						t.Fatal(err)
					}
				}
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			checksum := fmt.Sprintf("%x", sha256.Sum256(raw))
			if kind == "checksum" {
				checksum = strings.Repeat("0", 64)
			}
			curl := `#!/bin/sh
[ "$FAIL_DOWNLOAD" != 1 ] || exit 22
while [ "$#" -gt 0 ]; do
 if [ "$1" = -o ]; then shift; output=$1; fi
 shift
done
cp "$ARCHIVE_FIXTURE" "$output"
`
			if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0700); err != nil {
				t.Fatal(err)
			}
			githubPath := filepath.Join(dir, "github-path")
			if err := os.WriteFile(githubPath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			fail := "0"
			if kind == "download" {
				fail = "1"
			}
			cmd := exec.Command("bash", "-c", step["run"].(string))
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RUNNER_TEMP="+runner, "GITHUB_PATH="+githubPath, "UPSTATE_CLI_VERSION=0.2.13", "UPSTATE_CLI_SHA256="+checksum, "ARCHIVE_FIXTURE="+archive, "FAIL_DOWNLOAD="+fail)
			out, err := cmd.CombinedOutput()
			pathData, readErr := os.ReadFile(githubPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if kind == "success" {
				if err != nil {
					t.Fatalf("valid verified install failed: %v %s", err, out)
				}
				path := strings.TrimSpace(string(pathData))
				if path == "" {
					t.Fatal("verified binary not published")
				}
				info, err := os.Lstat(filepath.Join(path, "upstate"))
				if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
					t.Fatal("published unusable binary")
				}
			} else {
				if err == nil {
					t.Fatal("bad install incorrectly succeeded")
				}
				if len(pathData) != 0 {
					t.Fatal("failed install modified PATH")
				}
				if !strings.Contains(string(out), "::warning::Upstate CLI installation failed") {
					t.Fatalf("missing observable failure: %s", out)
				}
				entries, err := os.ReadDir(runner)
				if err != nil || len(entries) != 0 {
					t.Fatal("failed installation left files behind")
				}
			}
		})
	}
}

func TestTailscaleIsScopedAndMandatory(t *testing.T) {
	cfg, err := config.Load("../../examples/two-config/reviewed/.kamal-autodeploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ToolVersion = "v0.1.8"
	cfg.Units[0].SSH.Hosts = []string{"100.101.102.103"}
	cfg.Units[0].Tailscale = &config.Tailscale{Tags: []string{"tag:deploy"}}
	raw, err := Workflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	for name, value := range tree["jobs"].(map[string]any) {
		job := value.(map[string]any)
		perms := job["permissions"].(map[string]any)
		enabled := name == "deploy-"+cfg.Units[0].ID
		if (perms["id-token"] == "write") != enabled {
			t.Fatalf("OIDC permission leaked or missing in %s", name)
		}
		connect, ssh := -1, -1
		for i, value := range job["steps"].([]any) {
			step := value.(map[string]any)
			if step["name"] == "Connect to Tailscale (ephemeral OIDC identity)" {
				connect = i
				if step["continue-on-error"] != nil {
					t.Fatal("network must fail closed")
				}
				if step["if"] != "${{ steps.guard.outputs.proceed == 'true' }}" {
					t.Fatal("doctor must connect too, only after guard")
				}
				with := step["with"].(map[string]any)
				if with["ping"] != strings.Join(cfg.Units[0].SSH.Hosts, ",") || with["authkey"] != "file:${{ steps.tailscale-key.outputs.authkey_path }}" {
					t.Fatal("wrong network targets")
				}
				if step["uses"] != strings.Fields(TailscaleAction)[0] {
					t.Fatal("action must be pinned")
				}
			}
			if step["name"] == "Verify selected checkout host pins" {
				if strings.Contains(step["run"].(string), " --require-tailscale --") != enabled {
					t.Fatal("selected-checkout transport requirement leaked or missing")
				}
			}
			if step["name"] == "SSH with the dedicated CI key and pinned host keys" {
				ssh = i
			}
		}
		if enabled && (connect < 0 || ssh <= connect) {
			t.Fatal("join must precede SSH")
		}
		if !enabled && connect >= 0 {
			t.Fatal("network opt-in leaked")
		}
	}
	if !strings.Contains(string(raw), "StrictHostKeyChecking yes") {
		t.Fatal("lost host verification")
	}
	if bin, err := exec.LookPath("actionlint"); err == nil {
		path := filepath.Join(t.TempDir(), "workflow.yml")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, "-shellcheck=", path).CombinedOutput(); err != nil {
			t.Fatalf("actionlint: %s: %v", out, err)
		}
	}
}

func TestPreviewCannotReachDeployJobsOrSecrets(t *testing.T) {
	cfg, err := config.Load("../../examples/two-config/reviewed/.kamal-autodeploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ToolVersion = "v0.1.9"
	raw, err := Workflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	events := tree["on"].(map[string]any)["pull_request"].(map[string]any)["types"].([]any)
	foundEdited := false
	for _, event := range events {
		foundEdited = foundEdited || event == "edited"
	}
	if !foundEdited {
		t.Fatal("PR title/body edits must refresh the preview")
	}
	jobs := tree["jobs"].(map[string]any)
	preview := jobs["preview"].(map[string]any)
	perms := preview["permissions"].(map[string]any)
	if len(perms) != 1 || perms["contents"] != "read" || preview["environment"] != nil {
		t.Fatal("preview has privileged access")
	}
	previewYAML, _ := yaml.Marshal(preview)
	for _, forbidden := range []string{"secrets.", "GITHUB_TOKEN", "pull_request_target", "bundle exec", "tailscale-key", "guard --", "deploy --"} {
		if strings.Contains(string(previewYAML), forbidden) {
			t.Fatalf("preview includes %s", forbidden)
		}
	}
	if jobs["select"].(map[string]any)["if"] != "${{ github.event_name != 'pull_request' }}" {
		t.Fatal("PR can enter deploy selection")
	}
	edge := jobs["deploy-"+cfg.Units[1].ID].(map[string]any)
	if !strings.Contains(edge["if"].(string), "outputs.held != 'true'") {
		t.Fatal("dependency can bypass a newly established hold")
	}
}

func TestOlderToolPinKeepsCompatibleWorkflow(t *testing.T) {
	cfg, err := config.Load("../../examples/two-config/reviewed/.kamal-autodeploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ToolVersion = "v0.1.9-0.20261110120000-abcdefabcdef"
	raw, err := Workflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, unsupported := range []string{"pull_request:", "preview --base", "outputs.held"} {
		if strings.Contains(string(raw), unsupported) {
			t.Fatalf("old pin emits %s", unsupported)
		}
	}
	cfg.ToolVersion = "v0.1.9"
	raw, err = Workflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "preview --base") {
		t.Fatal("new pin lacks preview")
	}
}
