package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tailscaleKeyCommand exchanges GitHub's OIDC identity for a single-use,
// ephemeral node key. Credentials travel only in HTTPS bodies/headers and a
// mode-0600 runner temp file, never process arguments or diagnostic output.
func tailscaleKeyCommand(e *Env, args []string) error {
	f := newFlags("tailscale-key")
	tags := f.String("tags", "", "")
	if err := f.Parse(args); err != nil || f.NArg() != 0 || *tags == "" {
		return errors.New("usage: kamal-autodeploy tailscale-key --tags tag:deploy")
	}
	for _, name := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "TS_CLIENT_ID", "TS_AUDIENCE", "RUNNER_TEMP", "GITHUB_OUTPUT"} {
		if strings.TrimSpace(e.Getenv(name)) == "" {
			return fmt.Errorf("Tailscale OIDC requires runner variable %s", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	key, err := tailscaleNodeKey(ctx, client, e.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), e.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"), e.Getenv("TS_CLIENT_ID"), e.Getenv("TS_AUDIENCE"), strings.Split(*tags, ","))
	if err != nil {
		return err
	}
	base := e.Getenv("RUNNER_TEMP")
	if !filepath.IsAbs(base) || strings.ContainsAny(base, "\r\n") {
		return errors.New("invalid runner temporary directory")
	}
	dir, err := os.MkdirTemp(base, "kad-tailscale-")
	if err != nil {
		return errors.New("cannot create Tailscale key directory")
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, "authkey")
	if err := os.WriteFile(path, []byte(key), 0600); err != nil {
		return errors.New("cannot store temporary Tailscale key")
	}
	// Publish only the file path. Even GitHub masking commands must not emit keys.
	out, err := os.OpenFile(e.Getenv("GITHUB_OUTPUT"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return errors.New("cannot publish temporary key path")
	}
	_, writeErr := fmt.Fprintln(out, "authkey_path="+path)
	closeErr := out.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot publish temporary key path")
	}
	keep = true
	return nil
}

func tailscaleNodeKey(ctx context.Context, client *http.Client, requestURL, requestToken, clientID, audience string, tags []string) (string, error) {
	u, err := url.Parse(requestURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || !strings.HasSuffix(u.Hostname(), ".actions.githubusercontent.com") {
		return "", errors.New("invalid GitHub OIDC request endpoint")
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	var identity struct {
		Value string `json:"value"`
	}
	if err := tailscaleJSON(ctx, client, "GET", u.String(), requestToken, "", nil, &identity); err != nil {
		return "", fmt.Errorf("GitHub OIDC request: %w", err)
	}
	if identity.Value == "" {
		return "", errors.New("GitHub OIDC response omitted identity token")
	}
	var access struct {
		Token string `json:"access_token"`
	}
	body := url.Values{"client_id": {clientID}, "jwt": {identity.Value}}.Encode()
	if err := tailscaleJSON(ctx, client, "POST", "https://api.tailscale.com/api/v2/oauth/token-exchange", "", "application/x-www-form-urlencoded", []byte(body), &access); err != nil {
		return "", fmt.Errorf("Tailscale identity exchange: %w", err)
	}
	if access.Token == "" {
		return "", errors.New("Tailscale response omitted access token")
	}
	payload := map[string]any{"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{"reusable": false, "ephemeral": true, "preauthorized": true, "tags": tags}}}, "expirySeconds": 300, "description": "kamal-autodeploy temporary runner"}
	raw, _ := json.Marshal(payload)
	var node struct {
		Key string `json:"key"`
	}
	if err := tailscaleJSON(ctx, client, "POST", "https://api.tailscale.com/api/v2/tailnet/-/keys", access.Token, "application/json", raw, &node); err != nil {
		return "", fmt.Errorf("Tailscale node authorization: %w", err)
	}
	if node.Key == "" || strings.ContainsAny(node.Key, "\r\n") {
		return "", errors.New("Tailscale response omitted a valid node key")
	}
	return node.Key, nil
}

// Do not return provider bodies or transport errors: they can contain credentials.
func tailscaleJSON(ctx context.Context, client *http.Client, method, endpoint, bearer, contentType string, body []byte, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create HTTPS request")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("HTTPS request failed (details withheld)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d (response withheld)", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
		return errors.New("invalid JSON response (details withheld)")
	}
	return nil
}
