package cli

import (
	"fmt"
	"github.com/Upstate-run/kamal-autodeploy/internal/config"
	"github.com/Upstate-run/kamal-autodeploy/internal/generate"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadyUsesActualSecretProviders(t *testing.T) {
	for _, kind := range []string{"missing-ghcr-auth", "tracked-literal"} {
		t.Run(kind, func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, config.FileName)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing-ghcr-auth" {
				cfg.Units[0].RegistryPassword = config.RegistryNone
			} else {
				cfg.Units[0].Secrets = nil
				if err := os.WriteFile(filepath.Join(dir, ".kamal/secrets"), []byte("KAMAL_REGISTRY_PASSWORD=$KAMAL_REGISTRY_PASSWORD\nRAILS_MASTER_KEY=fixture-only\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			err = Run(e, []string{"ready", "--offline"})
			if kind == "missing-ghcr-auth" {
				if err == nil {
					t.Fatal("readiness hid the GHCR password requirement behind inferred github_token policy")
				}
			} else {
				if err != nil {
					t.Fatalf("rejected a tracked file-supplied secret: %v\n%s", err, out)
				}
				if strings.Contains(out.String(), "fixture-only") {
					t.Fatal("printed literal value")
				}
			}
		})
	}
}

func TestReadyRecognizesOptionalWorkflowProviders(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
				t.Fatal(err)
			}
			common := filepath.Join(dir, ".kamal/secrets-common")
			if err := os.WriteFile(common, []byte("SSH_AUTH_SOCK=$SSH_AUTH_SOCK\nSSH_AGENT_PID=$SSH_AGENT_PID\nUPSTATE_HOOK_TOKEN=$UPSTATE_HOOK_TOKEN\nUPSTATE_HOOK_ENVIRONMENT=$UPSTATE_HOOK_ENVIRONMENT\nUPSTATE_URL=$UPSTATE_URL\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", "-f", ".kamal/secrets-common")
			path := filepath.Join(dir, config.FileName)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if enabled {
				cfg.Upstate = &config.Upstate{CLIVersion: "0.2.13", SHA256: strings.Repeat("a", 64)}
			}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			err = Run(e, []string{"ready", "--offline"})
			if enabled && err != nil {
				t.Fatalf("workflow providers rejected: %v\n%s", err, out)
			}
			if !enabled && err == nil {
				t.Fatal("accepted Upstate providers when workflow integration is disabled")
			}
			if enabled {
				wf, err := generate.Workflow(cfg)
				if err != nil {
					t.Fatal(err)
				}
				var tree map[string]any
				if err := yaml.Unmarshal(wf, &tree); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, j := range tree["jobs"].(map[string]any) {
					job := j.(map[string]any)
					for _, s := range job["steps"].([]any) {
						step := s.(map[string]any)
						name, _ := step["name"].(string)
						if !strings.HasPrefix(name, "Run Kamal (") {
							continue
						}
						found = true
						jobEnv := job["env"].(map[string]any)
						stepEnv := step["env"].(map[string]any)
						for _, name := range generate.KamalProvidedVariables(cfg, cfg.Units[0]) {
							if _, ok := jobEnv[name]; ok {
								continue
							}
							if _, ok := stepEnv[name]; ok {
								continue
							}
							exported := false
							for _, earlier := range job["steps"].([]any) {
								earlierStep := earlier.(map[string]any)
								if earlierStep["name"] == step["name"] {
									break
								}
								run, _ := earlierStep["run"].(string)
								if strings.Contains(run, fmt.Sprintf(`echo "%s=$%s" >> "$GITHUB_ENV"`, name, name)) {
									exported = true
								}
							}
							if !exported {
								t.Errorf("managed provider %s is absent from generated Kamal environment and prior GITHUB_ENV exports", name)
							}
						}
					}
				}
				if !found {
					t.Fatal("no Kamal step verified")
				}
			}
		})
	}
}

func TestReadyTreatsMultilineProvidersAsUnknown(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, config.FileName)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Units[0].Secrets = nil
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(dir, ".kamal/secrets")
	if err := os.WriteFile(secretPath, []byte("KAMAL_REGISTRY_PASSWORD=$KAMAL_REGISTRY_PASSWORD\nRAILS_MASTER_KEY=\"first\n${CI_MASTER_KEY}\n\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err != nil {
		t.Fatalf("uncertainty was made a definite failure: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "unsupported quoted or multiline syntax") {
		t.Fatal("multiline provider was silently accepted as literal")
	}
	if strings.Contains(out.String(), "CI_MASTER_KEY") {
		t.Fatal("printed secret expression")
	}
}

func TestReadyRequiresImplicitRegistryPassword(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, config.FileName)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Units[0].RegistryPassword = config.RegistryNone
	cfg.Units[0].Secrets = nil
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".kamal/secrets"), []byte("RAILS_MASTER_KEY=fixture-only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deployPath := filepath.Join(dir, "config/deploy.yml")
	raw, err = os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	var deploy map[string]any
	if err := yaml.Unmarshal(raw, &deploy); err != nil {
		t.Fatal(err)
	}
	delete(deploy, "registry")
	raw, err = yaml.Marshal(deploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deployPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err == nil {
		t.Fatal("missed implicit registry password")
	}
	if !strings.Contains(out.String(), "newly discovered secret KAMAL_REGISTRY_PASSWORD") {
		t.Fatal(out.String())
	}
}

func TestReadyUsesKamalCommonSecretsPath(t *testing.T) {
	for _, destination := range []string{"", "production"} {
		for _, source := range []string{"custom-common", "unused-default"} {
			t.Run(destination+"/"+source, func(t *testing.T) {
				dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
				deployPath := filepath.Join(dir, "config/deploy.yml")
				raw, err := os.ReadFile(deployPath)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, []byte("\nsecrets_path: config/credentials\n")...)
				if err := os.WriteFile(deployPath, raw, 0644); err != nil {
					t.Fatal(err)
				}
				if destination != "" {
					if err := os.WriteFile(filepath.Join(dir, "config/deploy."+destination+".yml"), []byte("{}\n"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				selected := "config/credentials"
				if destination != "" {
					selected += "." + destination
				}
				if err := os.WriteFile(filepath.Join(dir, selected), nil, 0600); err != nil {
					t.Fatal(err)
				}
				git(t, dir, "add", "-f", selected)
				common := "config/credentials-common"
				if source == "unused-default" {
					common = ".kamal/secrets-common"
				}
				if err := os.WriteFile(filepath.Join(dir, common), []byte("KAMAL_REGISTRY_PASSWORD=$KAMAL_REGISTRY_PASSWORD\nRAILS_MASTER_KEY=fixture-only\n"), 0600); err != nil {
					t.Fatal(err)
				}
				git(t, dir, "add", "-f", common)
				e, out := testEnv(dir)
				if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				path := filepath.Join(dir, config.FileName)
				cfg, err := config.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				if len(cfg.Units) != 1 || cfg.Units[0].SecretsFile != selected {
					t.Fatalf("wrong selected secrets file: %+v", cfg.Units)
				}
				cfg.Units[0].Secrets = nil
				raw, err = yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0644); err != nil {
					t.Fatal(err)
				}
				out.Reset()
				err = Run(e, []string{"ready", "--offline"})
				if source == "custom-common" && err != nil {
					t.Fatalf("rejected actual custom common provider: %v\n%s", err, out)
				}
				if source == "unused-default" && err == nil {
					t.Fatal("accepted provider from a common file Kamal does not load")
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(raw) {
					t.Fatal("changed reviewed configuration")
				}
			})
		}
	}
}

func TestReadyRequiresFileEntriesForYAMLReferences(t *testing.T) {
	for _, name := range []string{"RAILS_MASTER_KEY", "KAMAL_REGISTRY_PASSWORD"} {
		t.Run(name, func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".kamal/secrets")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.ReplaceAll(string(raw), name+"=$"+name+"\n", ""))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := Run(e, []string{"ready", "--offline"}); err == nil {
				t.Fatal("an exported environment value was mistaken for a file entry")
			}
			if !strings.Contains(out.String(), "secret "+name+" has no tracked or generated secrets-file entry") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestReadyAcceptsGeneratedEntriesAndEnvironmentOnlyHookInputs(t *testing.T) {
	for _, generated := range []bool{false, true} {
		t.Run(fmt.Sprint(generated), func(t *testing.T) {
			dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
			if generated {
				git(t, dir, "rm", "--cached", ".kamal/secrets")
				f, err := os.OpenFile(filepath.Join(dir, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0644)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("\n.kamal/secrets\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			e, out := testEnv(dir)
			if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, config.FileName)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Units[0].Secrets = append(cfg.Units[0].Secrets, "HOOK_INPUT")
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := Run(e, []string{"ready", "--offline"}); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if generated {
				wf, err := generate.Workflow(cfg)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range generate.KamalSecretFileVariables(cfg.Units[0]) {
					if !strings.Contains(string(wf), fmt.Sprintf(`echo "%s=\$%s" >> "$SECRETS_FILE"`, name, name)) {
						t.Errorf("generated entry for %s is missing", name)
					}
				}
			}
		})
	}
}

func TestReadyKeepsAmbientRunnerReferencesUnverified(t *testing.T) {
	dir := fixtureRepo(t, "simple", "git@github.com:acme/shop.git")
	e, out := testEnv(dir)
	if err := Run(e, []string{"init", "--yes", "--direct-kamal", "--tool-version", "v0.1.5"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".kamal/secrets-common")
	if err := os.WriteFile(path, []byte("RUNNER_TEMP=$RUNNER_TEMP\nGITHUB_WORKSPACE=$GITHUB_WORKSPACE\nRUNNER_NOT_GUARANTEED=$RUNNER_NOT_GUARANTEED\nHOME=$HOME\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-f", ".kamal/secrets-common")
	out.Reset()
	if err := Run(e, []string{"ready", "--offline"}); err != nil {
		t.Fatalf("ambient context rejected as missing app secrets: %v\n%s", err, out)
	}
	for _, name := range []string{"RUNNER_TEMP", "GITHUB_WORKSPACE", "RUNNER_NOT_GUARANTEED", "HOME"} {
		if !strings.Contains(out.String(), name+" references ambient runner context; availability is not verified") {
			t.Fatalf("ambient availability was falsely established for %s: %s", name, out)
		}
	}
}
