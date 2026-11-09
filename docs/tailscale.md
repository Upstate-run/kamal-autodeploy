# Deploy over Tailscale with public SSH closed

Requires kamal-autodeploy v0.1.8 or newer. This optional per-unit integration connects selected deployment and remote-doctor jobs to a tailnet using GitHub OIDC. No long-lived Tailscale secret is stored in GitHub. Repositories without this option keep the ordinary SSH workflow and receive no OIDC permission.

## Server and network

Install and authenticate Tailscale on the deployment host using the official installation instructions. Keep ordinary OpenSSH running; do not enable Tailscale SSH for this integration. It uses the existing dedicated CI SSH key and pinned OpenSSH host key, carried over Tailscale. KAD uses direct HTTPS exchanges so identity/access/node keys never enter subprocess arguments, logs or tracked files; provider error bodies are withheld.

Use the host's stable Tailscale IPv4 address in **both** the Kamal server entries and the reviewed unit's `ssh.hosts`. Update any reporting mappings that match deployment target addresses. Preserve shared host locks and unit ordering. A transport-address change does not create a new physical server or require replacing its CI keys.

The provider firewall can block public TCP 22. Allow inbound UDP 41641 (or the server's configured Tailscale UDP port) for direct encrypted connections. Outbound HTTPS and Tailscale's documented outbound connectivity must be permitted. Without inbound UDP, relayed connectivity can still work, but may be slower. Do not allowlist the Tailscale 100.64.0.0/10 range in a provider firewall: those are inner tunnel addresses. On-host firewall rules must allow the intended SSH traffic on `tailscale0`.

Verify your own Tailscale SSH path before removing any existing administrative access. Keep the provider console available for recovery. Do not change application listener addresses, public DNS, proxy ports, or observer identity merely because SSH uses a different transport.

## Tailnet access policy

Create a dedicated runner tag, for example `tag:app-deploy`, owned by an administrator. Grant it access only to this server's Tailscale address and TCP 22 (or the configured SSH port):

```json
{
  "tagOwners": {"tag:app-deploy": ["autogroup:admin"]},
  "grants": [
    {"src": ["tag:app-deploy"], "dst": ["100.101.102.103"], "ip": ["tcp:22"]}
  ]
}
```

Merge these entries into the existing policy; do not replace unrelated rules. Grants and legacy ACLs are additive: an existing allow-all rule also grants this tag broad access. Restrict or replace such rules deliberately, preserving required administrator/device access. Use policy tests to confirm both allowed SSH and denied unrelated hosts/ports. No Tailscale SSH policy is needed because authentication remains OpenSSH.

## GitHub OIDC identity

In Tailscale's trust credentials settings create a federated OIDC identity for each GitHub deployment environment:

- Issuer: GitHub Actions (`https://token.actions.githubusercontent.com`).
- Subject: exact `repo:OWNER/REPO:environment:ENVIRONMENT`, matching the job's protected GitHub environment. This is the environment subject, not the branch subject.
- Scope: **Auth Keys / Write** only, restricted to the runner tag above.
- Audience: use the generated audience; record it and the client ID.

Use separate identities for separate environments. The subject binds the repository and environment, not a branch; retain KAD's main-only custom deployment branch policies in each GitHub environment. Do not broaden subjects to wildcard repositories or pull requests.

Store the client ID and audience as **environment variables** (these identifiers are not credentials):

```sh
gh variable set KAMAL_AUTODEPLOY_TAILSCALE_CLIENT_ID --repo OWNER/REPO --env ENVIRONMENT
gh variable set KAMAL_AUTODEPLOY_TAILSCALE_AUDIENCE --repo OWNER/REPO --env ENVIRONMENT
```

Both commands prompt for their value. The generated job receives `id-token: write`, exchanges the OIDC token for a single-use ephemeral node key, stores it in a mode-0600 runner temporary file, and connects using the SHA-pinned official Tailscale action with a `file:` key reference, waits for peer connectivity, and fails closed on errors. The key expires after five minutes if unused; a non-reusable key is consumed when the node joins. Job cleanup deletes the key file, and the action logs out its ephemeral node. Selection/summary jobs do not join the tailnet. The action's pinned default Tailscale client and cache are used; no second VPN installation path is maintained by KAD.

## Reviewed unit and host identity

Add to each relevant unit, alongside `ssh`:

```yaml
tailscale:
  tags: [tag:app-deploy]
ssh:
  user: deploy
  port: 22
  hosts: [100.101.102.103]
```

Set `tool_version` to a compatible published version, then regenerate with `kamal-autodeploy init`. Commit the Kamal configs, updated mappings, reviewed KAD config and generated workflow together. Keep activation manual for the first deployment.

Your workstation must also reach the tailnet for provisioning's host-key scan. Verify the server's OpenSSH fingerprint through a trusted existing connection or provider console (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`), then pin the **Tailscale address** with `provision --host-fingerprint '100.101.102.103=SHA256:...'`. Never substitute a scan for verification or bypass host-key checks. Existing authorized CI keys stay installed.

Run local `doctor`, then `doctor --remote --unit UNIT --yes`. Wait for success before deploying. A successful Tailscale ping proves peer connectivity, not SSH authorization or an allowed TCP port; remote doctor verifies the actual SSH login. After a manual deployment, verify live containers, health and any optional receipts before enabling automation.

The workflow passes its Tailscale requirement into selected-checkout verification. Older checkouts with public Kamal targets are rejected even when those addresses remain pinned. Rollback to an older application commit whose Kamal config still uses a public address fails this transport check. Keep private/public pins and network policy deliberate; do not bypass verification to force an old config through. Prefer rollback to a version already deployed with the private transport configuration.

References: [GitHub Action](https://tailscale.com/docs/integrations/github/github-action), [workload identity federation](https://tailscale.com/docs/features/workload-identity-federation), [firewall ports](https://tailscale.com/docs/reference/faq/firewall-ports).
