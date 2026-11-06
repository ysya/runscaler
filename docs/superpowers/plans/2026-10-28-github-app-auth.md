# GitHub App Authentication Implementation Plan

> **For agentic workers:** Execute inline with superpowers:executing-plans and verify with superpowers:verification-before-completion.

**Goal:** Support GitHub App authentication across runner commands while preserving PAT installations.

**Architecture:** Use the existing actions/scaleset SDK's GitHub App client. Keep credential references in per-scale-set configuration and centralize client construction; the SDK owns JWT and access-token exchange.

**Tech Stack:** Go 1.27.3, existing actions/scaleset v0.4.0, standard library crypto and tests.

**Spec:** User selected GitHub App authentication as the first project improvement.

## Global Constraints

- Work from main on `feat/github-app-auth`; preserve diagnostics work in its named stash.
- Add no dependencies and preserve existing PAT flags, environment fallback, and config behavior.
- App keys: `github-app.client-id`, `installation-id`, and exactly one of `private-key-file` or `private-key-env`.
- Private-key files use absolute paths. Configuration and CLI contain references, never PEM contents.
- App credentials never inherit across scale sets. App configuration suppresses ambient PAT fallback; explicit App plus PAT is invalid.
- Incomplete App configuration fails rather than silently falling back to PAT.
- Enterprise-level runners retain PAT authentication because GitHub Apps do not support that scope.
- Do not use real credentials, create a GitHub App, or deploy services. The user subsequently authorized committing, merging to main, and publishing v0.10.0 after CI succeeds.

## Review Focus

- A global PAT must not override a selected App, and partial App configuration must not change the selected mode.
- Multiple organizations must not inherit another scale set's App credentials or key source.
- Missing, unreadable, malformed, non-RSA or ambiguous private keys must fail without disclosing PEM contents.
- All command entry points must use the same auth selection, including non-interactive init.
- Tests must exercise the real SDK JWT-signing branch with generated keys and a fake HTTP transport.

### Task 1: Configuration and client

- [x] Add failing tests for auth validation, env fallback, non-inheritance, key sources, and SDK auth selection.
- [x] Add per-scale-set App config, structural validation, bounded private-key loading and RSA validation.
- [x] Centralize SDK App/PAT construction in a scale-set method with existing logger/retry behavior.
- [x] Run focused tests and the complete suite.

### Task 2: Command coverage and documentation

- [x] Add failing tests for App init output and run flag bindings.
- [x] Add App reference flags to run/init; share client construction across run/validate/doctor/scaleset delete.
- [x] Document single/multi-scale-set examples, permissions and key handling using GitHub's official docs.
- [x] Run full tests, race, vet, lint, vulnerability checks and cross-builds.

### Task 3: Review

- [x] Obtain an independent review and resolve material findings.
- [x] Report code and synthetic verification separately from live GitHub App acceptance.
