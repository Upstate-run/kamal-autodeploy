# Testing

`go test ./...` runs the local fixture and mock-API tests. No test deploys an application,
sets GitHub secrets, or contacts an SSH server. The two-service fixture uses documentation
addresses and example repository names; it is not a runnable production configuration.

Before using a generated workflow on infrastructure you own, review the plan, configure
branch-restricted environments and CI gates, verify SSH fingerprints independently, and
confirm the deployed version and rollback behavior. Keep real addresses and operational
runbooks outside the public repository. Upstate integration is optional.
