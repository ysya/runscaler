# Retire Legacy Rename Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans for inline implementation. Preserve the existing uncommitted installation-target work. No commit, release, live service mutation, or deletion of historical release assets is authorized.

**Goal:** Remove the historical runscaler-to-runner migration and compatibility layer, retaining current runner system/user installations and existing Linux root runtime identities.

**Architecture:** Delete migration-only code and release definitions; move the two still-used CLI path helpers into a small shared file. Keep the current config loader and platform managers, accepting canonical names only. Known retired config spellings fail with a clear error instead of silently changing providers or cache retention; unrelated unknown-key warnings retain their current contract.

**Tech Stack:** Existing Go/Cobra/Viper, GoReleaser, shell and Markdown; no new dependencies.

**Spec:** User explicitly selected removal of migrate, old binary assets, old config names and old environment-variable compatibility. This replaces the historical rename design; it does not withdraw support for a current runner.service that runs as root.

## Global Constraints

- Retire `runner migrate`, its config backup/canonicalization/cutover/cleanup implementation and tests, and future `runscaler-*` release archives. Do not delete remote or historical release assets.
- Retire root `runner --config` startup, `--backend`, implicit RUNSCALER_TOKEN fallback, deprecated TOML key translations, automatic runscaler config-directory search, old service-name handling and old log/resource-name fallbacks.
- Preserve `runner run`, flat and [[scaleset]] config, canonical provider/cache keys, explicit `token=env:NAME`, RUNNER_TOKEN, --scope and --user, current system/user layouts, shared lock, safe Docker/Tart cleanup and all current-runtime root protections.
- Preserve the project/module/repository name and the dedicated `runscaler` service account; these are current identifiers, not compatibility aliases.
- Preserve user data, configs, backups, containers, VMs and installed services. This task edits repository code/docs/release configuration only.
- Keep all prior worktree edits and leave the final combined changes uncommitted.

## Review Focus

- Removal must not orphan shellQuotePath/sameFilePath or other helpers now used by installation/update/logging.
- An old `backend=tart` must not silently turn into Docker after translation disappears; retired keys need explicit rejection in both flat and multi configurations.
- Current root runner services must retain their UID during --force/update; a user-mode installation must remain user-mode.
- No runtime message or command hint may recommend the removed migrate command, old flags or old service cleanup.
- Release checks must retain CGO_ENABLED=0 and all four supported platforms after removal of the shared compatibility build anchor.

## Task 1: Remove migration commands and compatibility binaries

- [x] Add negative tests for migrate registration, old startup invocation, deprecated flags and compat release definitions; observe failures.
- [x] Move shared path comparison/shell quoting helpers; delete cmd_migrate.go, migrate_config.go and migration-only tests/helpers.
- [x] Remove the root drop-in startup path and backend flags; remove future compatibility build/archive entries without changing primary build flags.
- [x] Adapt current service-template tests to the structured systemd/plist readers and preserve functional coverage after removing migration parsers.
- [x] Run focused CLI tests.

## Task 2: Remove configuration and runtime name fallbacks

- [x] Add failing tests for retired config keys at each scope, old automatic token fallback/config-directory search, legacy service argv and legacy resource prefixes.
- [x] Remove config aliases and unused compatibility struct fields; reject known retired keys without exposing values.
- [x] Remove old service-name detection/uninstall, old provider resource-name cleanup and log fallbacks. Keep current cleanup paths and root/user identity behavior.
- [x] Update fixtures using retired keys so positive tests use canonical names; retain explicit negative cases.
- [x] Run config/CLI/provider/layout/lock tests.

## Task 3: Document and validate retirement

- [x] Update both READMEs, help and active planning docs; mark historical rename plans as superseded. State that ancient rename-era installations require manual preparation for this version and current root runner installs remain supported.
- [x] Run go test -race ./..., go vet ./..., repository lint, CI govulncheck, four CGO-free builds, shell syntax, GoReleaser config check and git diff --check.
- [x] Request one fresh-context review of the retirement changes, fix material findings with regression tests, and rerun affected/full checks as required.
- [x] Report exact results and any live-host or release limitations. No commit/push/release.
