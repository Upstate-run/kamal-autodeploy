# Testing

`go test ./...` runs the local fixture and mock-API tests. No test deploys an application,
sets GitHub secrets, or contacts an SSH server. The two-service fixture uses documentation
addresses and example repository names; it is not a runnable production configuration.

Before using a generated workflow on infrastructure you own, review the plan, configure
branch-restricted environments and CI gates, verify SSH fingerprints independently, and
confirm the deployed version and rollback behavior. Keep real addresses and operational
runbooks outside the public repository. Upstate integration is optional.

Direct invocation and activation regressions cover standard and change-directory laptop wrappers remaining untouched, Bundler versus gem execution, explicit config/destination routing, mismatched/duplicate routing flags, unpinned overlay hosts, manual CI-trigger rejection, manual doctor/deploy behavior, required-CI failures, ordered prerequisite reconciliation and legacy automatic configuration compatibility. Generated-example tests and actionlint cover manual-first output; switching activation is tested through regeneration.

For a new repository's live acceptance: publish the manual workflow on the default branch, complete dedicated-key authorization and pinned host verification, require remote doctor success, explicitly deploy and verify the actual running version, then review/merge activation: automatic. Confirm a subsequent eligible CI completion deploys as expected. A mode change does not cancel already-running jobs.
