package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tailscaleTransport func(*http.Request) (*http.Response, error)

func (f tailscaleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTailscaleOIDCExchangeUsesHTTPSOnly(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: tailscaleTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" {
			t.Fatal("plaintext credential transport")
		}
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		var response string
		switch calls {
		case 1:
			if r.Header.Get("Authorization") != "Bearer request-secret" || r.URL.Query().Get("audience") != "example-audience" {
				t.Fatal("wrong GitHub identity request")
			}
			response = `{"value":"identity-secret"}`
		case 2:
			if r.URL.Host != "api.tailscale.com" || string(body) != "client_id=example-client&jwt=identity-secret" {
				t.Fatal("wrong identity exchange")
			}
			response = `{"access_token":"access-secret"}`
		case 3:
			if r.Header.Get("Authorization") != "Bearer access-secret" {
				t.Fatal("missing bearer")
			}
			var payload struct {
				Capabilities struct {
					Devices struct {
						Create struct {
							Reusable, Ephemeral, Preauthorized bool
							Tags                               []string
						}
					}
				}
				ExpirySeconds int
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			create := payload.Capabilities.Devices.Create
			if create.Reusable || !create.Ephemeral || !create.Preauthorized || len(create.Tags) != 1 || create.Tags[0] != "tag:deploy" || payload.ExpirySeconds != 300 {
				t.Fatalf("unsafe node capabilities: %s", body)
			}
			response = `{"key":"node-secret"}`
		default:
			t.Fatal("unexpected HTTP request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	key, err := tailscaleNodeKey(context.Background(), client, "https://vstoken.actions.githubusercontent.com/token", "request-secret", "example-client", "example-audience", []string{"tag:deploy"})
	if err != nil || key != "node-secret" || calls != 3 {
		t.Fatalf("exchange failed: %v", err)
	}
}

func TestTailscaleErrorsDoNotExposeCredentials(t *testing.T) {
	for _, status := range []int{0, 200, 302, 403, 500} {
		client := &http.Client{Transport: tailscaleTransport(func(r *http.Request) (*http.Response, error) {
			if status == 0 {
				return nil, errors.New("secret-provider-diagnostic")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("secret-provider-diagnostic"))}, nil
		})}
		_, err := tailscaleNodeKey(context.Background(), client, "https://vstoken.actions.githubusercontent.com/token", "request-secret", "client", "audience", []string{"tag:deploy"})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	for _, endpoint := range []string{"http://vstoken.actions.githubusercontent.com/token", "https://example.com/token", "https://vstoken.actions.githubusercontent.com.evil.test/token"} {
		_, err := tailscaleNodeKey(context.Background(), nil, endpoint, "secret", "client", "audience", nil)
		if err == nil {
			t.Fatal("untrusted identity endpoint accepted")
		}
	}
}

func TestTailscaleKeyRequiresRunnerIdentity(t *testing.T) {
	e, _ := testEnv(t.TempDir())
	e.Getenv = func(string) string { return "" }
	if err := Run(e, []string{"tailscale-key", "--tags", "tag:deploy"}); err == nil {
		t.Fatal("missing identity accepted")
	}
}

func TestTailscaleKeyFilePermissions(t *testing.T) {
	// Exercise the command through a substituted default transport; no subprocesses
	// or network calls can receive any credential.
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	http.DefaultTransport = tailscaleTransport(func(r *http.Request) (*http.Response, error) {
		response := `{"value":"identity-secret","access_token":"access-secret","key":"node-secret"}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	if err := os.WriteFile(output, nil, 0600); err != nil {
		t.Fatal(err)
	}
	e, out := testEnv(dir)
	vars := map[string]string{"RUNNER_TEMP": dir, "GITHUB_OUTPUT": output, "ACTIONS_ID_TOKEN_REQUEST_URL": "https://vstoken.actions.githubusercontent.com/token", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-secret", "TS_CLIENT_ID": "client", "TS_AUDIENCE": "audience"}
	e.Getenv = func(k string) string { return vars[k] }
	if err := Run(e, []string{"tailscale-key", "--tags", "tag:deploy"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(output)
	path := strings.TrimSpace(strings.TrimPrefix(string(raw), "authkey_path="))
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "node-secret" {
		t.Fatal("key not stored")
	}
	info, _ := os.Stat(path)
	parent, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0600 || parent.Mode().Perm() != 0700 {
		t.Fatal("key permissions are too broad")
	}
	if out.Len() != 0 || strings.Contains(string(raw), "secret") {
		t.Fatal("credential leaked to output")
	}
}
