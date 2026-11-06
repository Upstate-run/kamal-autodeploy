# kamal-autodeploy

**kamal-autodeploy is an independent open-source project. It is not made by, affiliated with or endorsed by 37signals or the Kamal project.** It generates GitHub Actions configuration that runs *your* Kamal, with *your* wrapper scripts, after *your* CI passes.

Merge a green pull request to `main`, and the Kamal services that change affects deploy on their own: a Rails-only change deploys Rails, an Edge-only change deploys Edge, a docs-only change deploys nothing, and a schema change deploys both, in order. It is continuous deployment that waits on your own CI. It is not a CI system, a hosted deploy service or a GitHub App.

- **No account, no service.** A small Go CLI writes two plain files into your repository: `.kamal-autodeploy.yml` (the reviewed plan) and `.github/workflows/kamal-autodeploy.yml` (ordinary, editable Actions YAML). Deploys never depend on this project being online beyond GitHub itself and the Go module proxy.
- **Correct change selection.** Each unit's baseline is its **last successful deploy**, recorded as a GitHub Deployment in that unit's own environment. A failed deploy, a skipped run or a docs-only merge never loses an undeployed change.
- **Conservative by default.** Unknown baseline: deploy. Files no unit claims: deploy every unit (or stop, if you choose). Missing or skipped CI is never success. Units sharing a host run one at a time.
- **Secrets stay between your machine, GitHub and your servers.** Provisioning goes through your own `gh` login; values travel only on stdin.

Works with [Upstate](https://upstate.cloud) for verified deploy receipts if you want it, and fully without it.

## Contents

- [Install](#install)
- [Readiness and onboarding output](#readiness-and-onboarding-output)
- [Quick start](#quick-start)
- [How a deploy is chosen](#how-a-deploy-is-chosen)
- [Configuration](#configuration)
- [Manual deploy and rollback](#manual-deploy-and-rollback)
- [SSH and secrets](#ssh-and-secrets)
- [Optional: Upstate receipts](#optional-upstate-receipts)
- [Private hosts](#private-hosts)
- [Supported and unsupported shapes](#supported-and-unsupported-shapes)
- [Security model](docs/threat-model.md)
- [Examples](#examples)

## Install

```sh
go install github.com/Upstate-run/kamal-autodeploy/cmd/kamal-autodeploy@v0.1.6
```

Published releases also provide macOS/Linux binary archives and `checksums.txt`; use those when Go is not installed locally and verify the archive checksum before use. Go installs use the module proxy and checksum database. The generated workflow pins the reviewed tool version. See [the release configuration](.goreleaser.yaml).

## Readiness and onboarding output

Before preparing a new repository, run:

```sh
kamal-autodeploy ready --direct-kamal
# Local assessment only, without GitHub requests:
kamal-autodeploy ready --offline --direct-kamal
```

`ready` reuses static discovery to list CI gates, Kamal runtime, required secret names and deployment units. It checks effective wrapper commands/arguments, rejects empty SSH target lists, and checks that current Kamal hosts/ports and SSH user match the reviewed SSH settings without rewriting that plan. It rejects duplicate CI gate names, required inputs missing from reviewed exports or known tracked file providers, and secrets-file tracking/ignore mismatches shared with doctor. It flags unresolved SSH targets, flags tracked secret commands (including `.kamal/secrets-common`), and identifies hooks and ERB whose local-file/environment dependencies need review. It never executes project code, secret commands, ERB or SSH, writes files, installs software, changes GitHub settings or dispatches workflows. Required Kamal inputs are checked before inferred registry-policy filtering. YAML secret lookups require a tracked or generated file entry as well as any backing environment export. Hook-only environment inputs do not require file entries. A tracked literal can supply an input without a CI export; command/interpolated providers and dynamic registry servers remain explicitly unverified. Ignored laptop files are not accepted as runner providers. Secret expressions and values are not printed. It prepares no app-specific edits: review those explicitly before using `init`.

With GitHub access it reuses doctor's checks for environment protections, secret presence and host pins. Missing **or inaccessible** environments are reported without assuming which caused the failure. GHCR metadata can establish package linkage, but does not prove the workflow's write permission. Output links to package settings and asks you to verify **Manage Actions access**; inherited permissions may already suffice. Add repository access only if missing. A failed metadata lookup never establishes that permission is absent.

Before `init`, `--branch` and `--direct-kamal` assess the proposed configuration. Once `.kamal-autodeploy.yml` exists, `ready` uses that reviewed configuration and refuses discovery overrides; use `doctor` for generated-workflow consistency. Exit status is nonzero for failed repository or GitHub setup checks, including setup not yet provisioned. Warnings/unknowns do not fail the command, but are not verified successes. For a new repository with missing GitHub setup, the next action is still to prepare/review the manual workflow before provisioning it. Offline checks cannot establish GitHub or runtime readiness.

Human-facing commands separate results and a **NEXT** action with blank lines. Green means verified, yellow marks warnings/unknowns/actions, and red marks failure. Labels remain readable without color. ANSI color is emitted only to a terminal, disabled by `TERM=dumb` or `NO_COLOR` (including an empty value), and never added to workflow machine output. A local doctor result does not prove runner SSH authorization; remote doctor dispatch does not prove its run succeeded. Failed local setup checks prevent remote dispatch.

## Quick start

From the root of a Kamal project with a GitHub `origin`:

```sh
kamal-autodeploy ready --direct-kamal # assess readiness; writes nothing
# Missing GitHub setup is expected on a new repo; follow the printed NEXT action.
kamal-autodeploy plan --direct-kamal  # detailed discovery + diff; writes nothing
kamal-autodeploy init --direct-kamal  # new configs default to activation: manual
git add .kamal-autodeploy.yml .github/workflows/kamal-autodeploy.yml
git commit -m "Prepare manual Kamal deployment"
kamal-autodeploy provision   # GitHub setup; execute the printed public-key authorization command
kamal-autodeploy doctor      # local + GitHub checks
# Push/review/merge these files onto the default branch. This manual workflow
# does not deploy when CI completes. Then, replacing shop with the planned unit:
kamal-autodeploy doctor --remote --unit shop
# Wait for the remote SSH check to pass, then explicitly deploy:
gh workflow run kamal-autodeploy.yml -f action=deploy -f unit=shop
# Verify production and the exact-version receipt if using Upstate. Then:
kamal-autodeploy init --activation automatic
# Review, commit and merge BOTH regenerated files to enable CI-triggered deployment.
```

`--direct-kamal` leaves laptop wrappers untouched and invokes the installed Kamal directly with explicit config/destination arguments. For `kamal: bundler`, it uses `bundle exec kamal` and the project's lockfile; `kamal: gem` uses the pinned installed gem. Wrapper-specific environment setup or other custom behavior is not carried into direct mode: express required behavior in reviewed Kamal configuration/hooks before opting in. Omit the flag to keep the existing wrapper path and its strict routing checks.

Activation and direct-init opt-in require v0.1.4 or newer (or a compatible source SHA). Explicit opt-in uses the running tool version unless `--tool-version` is supplied. New `init` configurations default to `activation: manual`. They have only `workflow_dispatch`; CI checks are still required for an explicit deployment. If required CI is still pending, manual-mode operators must dispatch again after it passes; there is no automatic follow-up run. Manual dispatch retains the existing ordered reconciliation of pending units, so it may deploy other units and prerequisites as described in the dispatch confirmation. The manual setting disables CI-triggered runs, not this explicit reconciliation.

Rediscovery preserves activation from a valid existing configuration; invalid prior configuration defaults safely to manual unless explicitly overridden. Configs from earlier versions that omit `activation` remain automatic when regenerated. Use `init --activation manual` to stop future CI-triggered runs, or `init --activation automatic` to enable them. Regenerate and commit both files together. Switching modes does not cancel runs already in progress, and automatic activation itself is an operator-reviewed change; the tool does not infer successful onboarding from an earlier health check.

`init` never deploys anything. Running it again regenerates the workflow from the config and shows a diff; it refuses to overwrite a workflow it did not generate, or one you edited by hand (use `--force` for the latter only).

The plan lists every deployment unit with the exact command it will run, its config and secrets file, its path ownership, shared paths, order, CI gates, GitHub environment and lock, and three kinds of notes:

- **Review**: a conservative default was applied (for example, "services/edge/ is inside rails's build context, so Edge-only changes deploy Rails too").
- **Unknown**: something that cannot be known without running project code (ERB, templated hosts or secrets). Discovery never evaluates ERB or runs your scripts.
- **Unsupported**: a shape the workflow cannot handle yet; `init` refuses until it is resolved.

## How a deploy is chosen

The generated workflow has one `select` job and one deploy job per unit. Manual activation omits the CI trigger and rejects CI-completion events at verification, selection and guard boundaries.

1. **Trigger.** When activation is automatic, `workflow_run` after your CI workflow(s) complete, plus `workflow_dispatch`. Before checkout, the run is verified: same repository, not a fork, a `push` event, branch `main`, an expected workflow name *and* path, and a 40-character head SHA. A green pull request check never deploys.
2. **Baseline.** For each unit, the newest GitHub Deployment (task `kamal-autodeploy`) in the unit's environment that ever reached `success`. GitHub's own environment deployments (task `deploy`) are ignored.
3. **Candidate.** The newest commit on main's first-parent history (up to 25 back, always including the trigger) that is newer than the baseline and whose **required workflows for that unit** all succeeded for that exact SHA on a push to main. Pending, failed, cancelled, skipped or absent runs are not success. If a newer commit is still in CI, the run waits for it (its completion starts another run). Picking the newest green commit is what makes a replaced queued run safe.
4. **Changes.** Files changed between the baseline and the candidate (`git diff --no-renames`, so a move counts for both sides). Ignore rules apply first, then the unit's `paths` and `shared` rules. Files no unit claims deploy every unit (`unclassified: deploy`, the default) or stop the run (`unclassified: stop`).
5. **Unknown means deploy.** No successful deploy yet, a history too long to read, or a baseline commit that no longer exists (force-push) all deploy the candidate, with the reason in the job summary.
6. **Order and locks.** Units deploy in `after` order; units that share a host (`lock`) are chained so they never run in parallel. A unit whose predecessor failed is skipped and keeps its baseline. Trusted deployment runs share one concurrency group with `cancel-in-progress: false`.
7. **Last check.** Each deploy job checks out the exact SHA, verifies `HEAD`, and re-reads the baseline right before Kamal: if another run deployed this commit or a newer one meanwhile, or the commit is no longer on main, it does not deploy (stale).
8. **Deploy.** `<your command> deploy --version=<sha>` with secrets from the unit's environment, then the Deployment is marked `success` or `failure`. Only success advances that unit.

Automatic failures are not retried by another CI completion for the same unit, deploy version and observed branch head. The serialized deploy guard checks GitHub Deployment records and prints why the duplicate attempt was skipped. Push a new commit or dispatch `action=deploy` for that unit to retry. A new docs-only commit can retry an older eligible version when that unit's CI is path-filtered. Explicit manual requests remain retryable; records from older tool versions without an automatic attempt key do not suppress work. Required CI must still pass before any attempt is created.

Scenarios covered by tests (`internal/selection`): first deploy, single config, destination overlays, two standalone configs with wrappers, shared host, docs-only, Rails-only, Edge-only, migration/shared paths, two rapid merges, queued-run replacement, CI failure (and skipped/cancelled CI), failed deploy then docs merge, partial success, manual deploy, rollback and hold, recovery after a failed rollback, stale runs, force-push, renames and deletes, path-filtered and absent CI, fork and pull-request runs, GitHub API errors and pagination.

## Configuration

`.kamal-autodeploy.yml` is plain YAML, strictly validated (unknown keys are errors). The examples show complete files: [simple](examples/simple/.kamal-autodeploy.yml), [two configs, as discovered](examples/two-config/.kamal-autodeploy.yml), [two configs, reviewed](examples/two-config/reviewed/.kamal-autodeploy.yml).

| Key | Meaning |
| --- | --- |
| `activation` | `manual` or `automatic`. New init configs are manual; omitted in legacy configs means automatic. |
| `branch` | The trusted deploy branch (must be the default branch). |
| `tool_version` | Exact kamal-autodeploy version the workflow installs (tag, pseudo-version or full SHA). |
| `ci[]` | `name` and `path` of each CI workflow that can gate a deploy. |
| `unclassified` | `deploy` (every unit) or `stop` for changed files no rule claims. |
| `ignore[]` | Paths that never deploy anything. Evaluated first. |
| `shared[]` | `paths` that deploy all listed `units`. |
| `units[].id` | Unit ID (also the dispatch input and job name). |
| `units[].environment` | GitHub environment holding this unit's secrets and deployed baseline. One per unit. |
| `units[].command`, `args` | A reviewed wrapper, or direct `kamal` with config/destination routing only. `init --direct-kamal` constructs explicit `-c`/`-d` arguments. |
| `units[].requires[]` | CI workflow names that must pass for a commit before this unit deploys it. |
| `units[].after[]`, `lock` | Ordering within a run; units with the same lock never deploy in parallel. |
| `units[].paths[]` | Path ownership. Globs with `**`; `!pattern` excludes; last match wins. |
| `units[].secrets_file`, `write_secrets_file`, `secrets[]` | Kamal secrets file; whether CI writes `NAME=$NAME` references into it (ignored files) or uses it as tracked; the secret names. |
| `units[].registry_password` | `github_token` (GHCR with the job's token), `secret` (`KAMAL_REGISTRY_PASSWORD`) or `none`. |
| `units[].kamal`, `kamal_version` | `bundler` (Gemfile.lock) or `gem` with an exact version. |
| `units[].ssh` | `user`, `port`, and every `hosts` entry, so each host key can be pinned. |
| `upstate` | Optional pinned upstate CLI for receipts (see below). |

Path patterns are relative to the repository root. `*.md` matches only root-level Markdown; `**/*.md` matches everywhere. Discovery derives ownership from each config's build context and `.dockerignore`, adds the unit's own config, wrapper, Dockerfile and secrets files, and removes other units' deploy files. It cannot infer contracts between units (a schema both read): add those to `shared`.

## Manual deploy and rollback

The workflow takes `workflow_dispatch` inputs `action` (`deploy`, `rollback`, `doctor`), `unit` and `version`:

```sh
gh workflow run kamal-autodeploy.yml -f action=deploy -f unit=rails
gh workflow run kamal-autodeploy.yml -f action=rollback -f unit=rails -f version=<previous sha>
```

With the Upstate CLI, `upstate deploy <unit>` and `upstate rollback <unit> <version>` run the same commands through your `gh` after a confirmation and print the run URL.

- **Deploy now** deploys the newest commit on main with passing required CI, even without changes.
- **Rollback** runs `<command> rollback <version>` from main's config. Afterwards the unit is **held**: automatic deploys skip it (they would redeploy the code you rolled back) until a manual deploy.
- **A failed rollback** leaves the running version unknown: automatic deploys stop for that unit and the run fails with an attention message until a manual deploy or a successful rollback.
- Deploys run outside this workflow (from a laptop) are not seen; the baseline stays the last deploy this workflow recorded, and the next automatic deploy may replace what you deployed by hand.

## SSH and secrets

There are two separate SSH trust steps, and both are required:

| Item | What it proves | Where it belongs |
| --- | --- | --- |
| Server host-key fingerprint | The runner reached the intended server | Pinned host keys in the GitHub environment |
| Generated CI public key | The runner may log in as the configured SSH user | That user's `~/.ssh/authorized_keys` on every target |

`provision` generates the CI pair and uploads its private half to GitHub. It **does not install the public half on the server**: execute the printed authorization command through an existing trusted SSH connection. Keep existing authorized keys. Supplying `--host-fingerprint` completes only the first row. Local `doctor` checks secret presence and host pins; only a successful remote doctor establishes runner login access. The remote doctor requires this workflow on the default branch; publish it with manual activation first, then verify connectivity and an explicit deployment before enabling automatic triggers. If an existing CI secret is kept, its original public key must still be installed; the tool cannot recover it from GitHub's write-only secret store. A missing original public key needs an intentional paired-key replacement, not a host-pin bypass.

`kamal-autodeploy provision` does this through your own `gh`, with a yes before each change. The same steps by hand:

1. **Environment** per unit (`production-rails`), deployable from `main` only. Add required reviewers if you want a manual gate; the deploy job honours them.
2. **A dedicated CI key**, never your own: `ssh-keygen -t ed25519 -N "" -C kamal-autodeploy-ci -f ./ci_key`, add `ci_key.pub` to the deploy user's `authorized_keys` on every host, store it with `gh secret set KAMAL_AUTODEPLOY_SSH_KEY --env production-rails < ci_key`, then delete `ci_key`. Prefer a dedicated deploy user; docker group membership is effectively root.
3. **Pin each host key.** Read the fingerprint out of band (provider console: `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`), compare with `ssh-keyscan -t ed25519 HOST | ssh-keygen -lf -`, and only if they match: `ssh-keyscan -t ed25519 HOST | gh variable set KAMAL_AUTODEPLOY_KNOWN_HOSTS --env production-rails`. The workflow uses `StrictHostKeyChecking yes` and refuses to run unless every configured host is pinned; there is no `StrictHostKeyChecking=no` anywhere.
4. **Secrets** named in the plan: `gh secret set NAME --env production-rails` reads the value from stdin. `provision --import .kamal/secrets.rails` imports literal values from an ignored file without sourcing it; values that use `$(...)` or `$VAR` are listed for you to set yourself. Existing secrets are kept unless you pass `--overwrite` and answer yes for each one in a terminal.
5. **Rails credentials** stay encrypted in the repository; CI only needs `RAILS_MASTER_KEY` (provision offers `config/credentials/production.key` or `config/master.key` when they are ignored).
6. **Registry.** GHCR images owned by the repository owner use the job's `GITHUB_TOKEN` (`packages: write` on the deploy job only). Other registries use a `KAMAL_REGISTRY_PASSWORD` secret.

In CI, a git-ignored secrets file (`.kamal/secrets.rails`) is written with `NAME=$NAME` lines only, so Kamal reads values from the step's environment; it is deleted at the end. A tracked secrets file (Rails 8 style, references only) is used as is; if it runs a command such as `$(cat config/master.key)`, change it to `$RAILS_MASTER_KEY`. External vaults keep working if their CLI is available on the runner; that is yours to add.

## Optional: Upstate receipts

If the project is connected to Upstate (committed `.upstate/*.json` and managed Kamal hooks), CI deploys can produce full Upstate receipts:

```sh
kamal-autodeploy init --upstate-cli 0.2.13   # pins the upstate CLI version + SHA-256 from Upstate-run/cli-releases
kamal-autodeploy provision                   # offers to store the environment's hook token (interactive yes only)
```

The hook token is Upstate's own report-only credential, scoped to one environment and revocable; no SSH key or app secret reaches Upstate. It is piped from `upstate credential hook` straight into `gh secret set UPSTATE_HOOK_TOKEN`, and `UPSTATE_HOOK_ENVIRONMENT`/`UPSTATE_URL` become environment variables. The deploy job then installs the pinned, checksum-verified CLI. Everything here is skippable: without it, the hooks print one line and the deploy continues, and Upstate sees deploys only as observed version changes. Upstate being down never stops a deploy.

## Private hosts

GitHub-hosted runners reach only public addresses. Hosts that look private (RFC 1918, `100.64.0.0/10` as used by Tailscale, `.ts.net`, `.local`, `.internal`, single-label names) are reported as unsupported. Options: expose SSH on a public address restricted to [GitHub's runner ranges](https://api.github.com/meta), use a bastion (not supported by the generated workflow yet), or run your own runner with network access (not recommended on the production host itself, and not generated). VPN/OIDC integration is out of scope for v1.

## Supported and unsupported shapes

Supported: `config/deploy.yml` (or `deploy.yml`), destination overlays (`deploy.<dest>.yml`, one unit each), several standalone configs (`deploy.rails.yml`, `deploy.edge.yml`) with or without wrapper scripts, custom `secrets_path`, builder contexts and Dockerfiles, shared hosts and proxies, GHCR, Docker Hub and other registries with a password, bundler or gem Kamal.

Not supported yet (reported by `plan`): `ssh.proxy` jump hosts, private-network hosts, hosts or service names only known through ERB, CI workflows that are all path-filtered, a deploy branch that is not the default branch, more than one generated workflow per repository. A wrapper that also needs `-d` is flagged for review. Kamal hooks run as in any Kamal deploy; Kamal's own SSH settings (`ssh.keys`, `ssh.config`) are yours to keep consistent with the CI key.

## Examples

- [`examples/simple`](examples/simple): a conventional single-config Rails app (from [`fixtures/simple`](fixtures/simple)).
- [`examples/two-config`](examples/two-config): a two-service app with two standalone configs and wrappers on one host and kamal-proxy, as discovered and [after review](examples/two-config/reviewed/.kamal-autodeploy.yml) (from [`fixtures/two-services`](fixtures/two-services), with generic documentation-only addresses; see [testing](docs/testing.md)).

## Development

```sh
go vet ./... && go test ./...
go test ./internal/cli -update   # regenerate examples/ after an intentional generator change
```

MIT licensed. See [LICENSE](LICENSE).

### Review safety guarantees

Every trusted push CI completion reconciles the newest green commit, including failed
completions: a failed later CI run cannot replace a queued run and strand an earlier
green change. Rejected events use separate concurrency groups. Manual deploy and rollback
also reconcile pending changes for other units in the same serialized run. A doctor
request uses a separate group, checks SSH only, and never deploys any unit.

Before running Kamal, the workflow statically re-discovers the selected checkout's
config/destination and command routing and requires every actual target to be pinned.
Unresolved ERB targets, changed wrapper routing, or missing configs fail closed.
Existing environments must have a single custom branch policy for the deploy branch,
with no wildcard or tag policies; provisioning refuses unsafe environments before
accessing secrets and leaves reviewer protections intact. Environment names are compared
case-insensitively. A failed recovery deploy never clears a failed-rollback hold.

Only simple, statically understood command routing is accepted by CI host verification:
a direct `kamal` command with the discovered arguments, a minimal Bundler binstub, the
standard Bundler-generated Kamal binstub when no sibling `bin/bundle` loader exists, or a
single `exec kamal "$@"` shell wrapper with an optional literal `-c` config. Wrappers may
use the repository-root assignment shown in the examples. Other shell logic is refused;
comments never determine which config a wrapper executes. SSH ports must be literal integers.

Suppression of a previously failed automatic attempt returns a blocking guard error: the unit job remains failed and its dependants cannot deploy. It creates no new deployment record and does not run Kamal. Regression coverage includes two reconciliation runs after a shared-schema predecessor failure.

Executable wrappers must begin with a plain supported Ruby or shell shebang. Interpreter options and alternate loader programs are rejected before host pins are approved.

### First repository onboarding checks

Before the first production run:

- Authorize the generated CI public key for the configured SSH user on every target, separately from verifying server host fingerprints. Require remote doctor to pass once the workflow is on the default branch.
- Review the **committed** `.upstate/*.json` mapping when enabling receipts. Its environment ID and origin must match the configured hook environment and origin. A correct local mapping that was not committed cannot configure a fresh runner; hook credentials deliberately refuse mismatched environments.
- Review both `.kamal/secrets` and `.kamal/secrets-common` for laptop-only credential commands. A fresh runner cannot use credentials stored only in the developer's local Upstate machine store. Remove obsolete observer lookups only after confirming they are no longer needed by the app's deployment topology.
- Preserve a supported Kamal local registry/remote builder setup rather than switching registries solely for CI. Exclude runner dependency caches such as `vendor/bundle/` from the Docker context when applicable.
- After the first green deployment, verify an Upstate receipt for that exact deployed version and timestamp. An older verified receipt is not evidence for the new deployment. A successful Kamal job alone does not establish hook delivery, notification delivery, or independent production verification. If hooks fail open, investigate their reporting status separately.
