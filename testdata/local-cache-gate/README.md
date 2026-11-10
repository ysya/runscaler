# Real-job cache gate fixture

This is an unpublished review fixture, not an active repository workflow.
It requires explicit approval of the canary repository and temporary runner
registration. Copy this directory and acceptance.yml into an isolated branch;
acceptance.yml goes under .github/workflows/local-cache-gate.yml.

Register only a temporary classic runner with the exact unique label
runscaler-local-cache-gate. Use the pinned fork image in the approved isolated
Docker daemon, an independently reachable private cache endpoint, and a separate
throwaway data volume. Run the service with normal GitHub token validation and
Results passthrough, not the controlled issuer from cache-protocol-fixture.mjs.
Do not modify existing scale sets or their configuration. Do not use a renamed
production context as isolation.

The first Node action requires CUSTOM_ACTIONS_RESULTS_URL, verifies the actual
Results endpoint and v2 mode, and authenticates a missing-key lookup before
any cache payload action can execute. It exports the verified nonsecret endpoint
for the later container fixture. Container actions explicitly request v2 mode
because the candidate container handler does not set that flag itself.

The two jobs exercise Node, composite and container handlers and their post
steps (including explicit payload writes from all three post handlers and
exact-byte restores on the next fresh runner), the actual cache action's post-save followed by a separate-job restore,
and ordinary artifact upload/download through the service passthrough. All
actions are pinned to commits resolved from their release tags on 2026-11-10.
No cloud cache baseline is created. Artifacts intentionally still use GitHub.

This fixture does not prove Go, Node package-manager, Gradle, Rust or BuildKit
compatibility, full traffic accounting, self-update handling, or amd64/arm64
parity. Those checks remain separate. Review job logs and actual local server
requests before accepting any image. Never print runtime tokens or payload URLs.

Validate the review fixture with:

```sh
actionlint -config-file testdata/local-cache-gate/actionlint.yaml testdata/local-cache-gate/acceptance.yml
```
