# Runner Diagnostics Implementation Plan

> **For agentic workers:** Use superpowers:dispatching-parallel-agents for the independent doctor and runtime-health scopes; verify with superpowers:verification-before-completion.

**Goal:** Make runner startup failures and GitHub API connectivity visible to operators.

**Architecture:** Extend the existing controller telemetry, health JSON and status renderer. Use the existing scaleset SDK for read-only doctor probes without changing authentication or scheduling.

**Tech Stack:** Go 1.27.3, existing scaleset SDK, standard library testing.

**Spec:** The user's request to begin the project improvements discussed in this chat; first increment is health and doctor diagnostics.

## Global Constraints

- Start from current main and keep changes on `feat/runner-diagnostics`.
- Add no dependencies and do not change provider lifecycle or retry policy.
- Normal capacity waits, scale-to-zero, and lack of demand are not failures.
- Health polling remains in-memory; do not perform provider or GitHub I/O on HTTP requests.
- Do not expose tokens in new diagnostics or alter cleanup safeguards.
- The user authorized commit, merge to main and a new release after tests and CI succeed. Do not deploy or restart installed services.

## Review Focus

- API checks must probe every configured scale set, including those after a failed probe.
- Doctor must distinguish API failure from a clean orphan report, and report only safe error categories without raw response bodies or credentials.
- A missing scale set is a valid pre-registration state; doctor must not create or update it.
- Startup cancellation during shutdown must not count as an operational failure.
- Failed startup must degrade readiness only while unmet demand remains; success must restore it.

### Task 1: Make doctor probe the API

- [x] Add failing tests for all-scale-set probing, missing registration, failure continuation, deadlines, token redaction, and exit status.
- [x] Query runner groups and scale sets using existing read-only SDK calls, with bounded per-scale-set contexts.
- [x] Keep checking local resources after API failures and return a nonzero exit status when diagnostics fail.
- [x] Run focused tests and the complete suite.

### Task 2: Report runtime supply health

- [x] Add failing tests for startup failures, success recovery, capacity waits, cancellation, zero demand, and status output.
- [x] Record startup attempts, failures, safe failure categories, last successful startup, capacity waiting, and draining.
- [x] Add compatible health JSON fields and display them in runner status; derive readiness from connection and runtime state.
- [x] Run focused tests, full race suite, vet, lint, and vulnerability checks.

### Task 3: Review and document

- [x] Update README with the exact health and doctor behavior.
- [x] Obtain an independent review and resolve material findings.
- [ ] Commit and open a PR; merge after CI, wait for main CI, publish a new minor release, and verify all assets.
