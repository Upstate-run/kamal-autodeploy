# Threat model

What kamal-autodeploy protects, from whom, and where its guarantees end. It complements the security notes in the [README](../README.md).

## Assets

1. **Production hosts**: SSH access as the deploy user (with docker group membership this is effectively root).
2. **Application secrets** in each unit's GitHub environment, and the Kamal registry credential.
3. **The deployed version**: production must only ever run a commit that passed CI on `main`.
4. **The deploy record**: per-unit GitHub Deployments that decide what deploys next.

## Trust boundaries

| Party | Trusted for | Not trusted for |
| --- | --- | --- |
| The user's workstation and `gh` login | Running `init`, `provision`, `doctor`; reading ignored secrets files | Being online for deploys |
| GitHub (Actions, environments, Deployments API) | Holding secrets, running the deploy job, recording deploys | Fork pull requests, artifacts of other workflows |
| Commits on `main` that passed CI | Code that is deployed and executed with secrets | - |
| Pull requests, forks, other branches | Nothing | Never reach a job that has secrets |
| This project (its releases and module) | Code installed at an exact, checksum-verified version | Being online; holding anything |
| Upstate (optional) | Receiving deploy reports with its own report-only token | SSH keys, app secrets, builds; being online |

## Threats and mitigations

**A pull request or fork tries to deploy or read secrets.** The deploy workflow never runs on `pull_request` or `pull_request_target`. `workflow_run` is accepted only when the CI run's repository and head repository equal this repository, the event is `push`, the branch is `main`, the workflow name and path are expected. The SHA is then re-checked against `main`'s history (`git merge-base --is-ancestor`) before anything is selected. No artifacts from the CI run are downloaded; the deploy job checks out the commit itself.

**Deploying a commit whose CI did not pass.** Each unit requires named workflows; selection reads their runs for the exact SHA through the API and accepts only a completed `success` of a push run in this repository. Pending, failed, cancelled, skipped, neutral and absent runs are not success. Manual "deploy now" uses the same rule.

**Deploying a stale commit or rolling production back by accident.** Selection measures from each unit's last successful deploy and picks the newest green commit; a run whose commit is older than (or equal to) what is deployed skips it. The deploy job re-reads the baseline after taking the concurrency lock and skips if another run deployed this commit or a newer one, or if the commit is no longer on `main`. Trusted deployment runs share one concurrency group with `cancel-in-progress: false`, so a running deploy is never cancelled by a newer one.

**Losing an undeployed change.** Baselines advance only on a recorded `success`; a failed, cancelled or skipped deploy keeps the old baseline, so the next run includes the change. Unknown baselines and unclassified files deploy rather than skip.

**Parallel deploys on a shared kamal-proxy.** Units with the same lock are chained within a run; runs are serialized by the concurrency group. Another workflow that also deploys with Kamal is flagged by `doctor` because it would bypass this.

**Man-in-the-middle on SSH.** Host keys are pinned in `KAMAL_AUTODEPLOY_KNOWN_HOSTS` only after the user confirms a fingerprint obtained out of band (typing it, or `--host-fingerprint`); `ssh-keyscan` output is only a candidate and a mismatch stops provisioning. The deploy job writes `StrictHostKeyChecking yes` and refuses to start unless every configured host is pinned. There is no `StrictHostKeyChecking=no`. Limitation: Kamal's SSH library applies `~/.ssh/config` only when the Kamal config enables it; with Kamal's default it accepts keys for hosts that are *not* in `known_hosts`, which is why the job refuses unless every host is listed.

**Secret exposure.** Secret values are passed to `gh secret set` on stdin, never argv. The CI private key is generated in a 0700 temporary directory and deleted after it is stored. Imports read only literal values from git-ignored, untracked files, never source or evaluate them, and never print values. Existing secrets are not replaced without `--overwrite` and an interactive yes. In the workflow, each secret is mapped only into the Kamal step's environment, the written secrets file contains only `NAME=$NAME` references and is deleted afterwards, and the job summary contains SHAs, unit IDs and reasons only.

**Excess token permissions.** The workflow sets `permissions: {}` and grants per job: `select` gets `contents: read`, `actions: read`, `deployments: read`; each deploy job gets `contents: read`, `deployments: write`, and `packages: write` only when GHCR uses `GITHUB_TOKEN`. Checkouts use `persist-credentials: false`.

**Supply chain.** Actions are pinned to full commit SHAs of releases at least two weeks old. The tool is installed with `go install module@exact-version`, verified by the Go checksum database. The optional upstate CLI is downloaded over HTTPS from `Upstate-run/cli-releases` and checked against a SHA-256 pinned in the config at `init` time (read from that release's `checksums.txt`, so it protects against later tampering, not a malicious release). Kamal comes from your Gemfile.lock or an exact `gem install -v`.

**Malicious or confusing repository content during discovery.** Discovery parses YAML with ERB replaced by a marker, caps file sizes, refuses symlinks, never runs project scripts and validates every value that reaches the generated workflow against strict patterns (commands, arguments, host names, secret names, paths). Unit inputs reach shell steps through environment variables, never through `${{ }}` interpolation into scripts; dispatch versions are validated twice.

**Overwriting hand-written work.** `init` refuses any workflow at the generated path that it did not generate, and any generated workflow whose checksum shows a hand edit (`--force` covers only the latter).

## Out of scope

- A compromised `main` branch or CI workflow: code that reaches `main` and passes CI runs with production secrets by design. Use branch protection, required reviews and environment reviewers.
- A compromised GitHub account with admin access to the repository or its environments.
- Out-of-band deploys from a laptop are not recorded and can be replaced by the next automatic deploy.
- Private networks, VPNs and self-hosted runners.
- Signing of release artifacts (checksums only, like the Upstate CLI).

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
