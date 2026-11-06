# Installation Target and Non-Root Service Plan

> Historical record: rename migration and compatibility requirements in this document were superseded by [legacy retirement](../plans/2026-11-07-retire-legacy-compatibility.md). Do not execute the retired migration/compatibility steps. Current runner root/user installation behavior remains supported.


> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans for inline implementation, or superpowers:subagent-driven-development only if delegation is selected. Follow the linked design and execute tasks in order. The user approved updating the planning direction; this document does not authorize implementation, commits, account changes, live migrations, or service restarts.

**Goal:** Default new Linux installations to system scope with a dedicated non-root runtime account; preserve existing Linux root runtimes, macOS user mode and an explicit Linux user installation option, with consistent management targets.

**Architecture:** Keep the existing service-manager adapters, one-host lock, config loader, and updater. Add a shared installation-target resolver, explicit service-scope layout, and validated Linux account/directory preparation for new installations. Existing Linux root services remain supported without identity conversion.

**Tech Stack:** Existing Go/Cobra/Viper/Moby code, Linux systemd/account utilities and macOS launchd; no new runtime dependency for this project.

**Spec:** [Installation target and identity design](../specs/2026-11-07-installation-target-design.md). The deployment direction is confirmed; existing Linux root runtimes are preserved, and the dedicated non-root default applies to new system installations.

## Global Constraints

Implementation increment (2026-11-07): the user authorized installation modes,
non-root execution and CLI target consistency, followed by tests. Tasks 1–4
are implemented in an isolated worktree; the former Task 5 is withdrawn.
Existing root services retain their identity. No live account/service operation,
commit, push or release was performed. Automated tests cover local behavior;
the authorized fixture-host gates below remain unchecked.

- Linux new installations default to system scope, with root-owned binary/config and a dedicated non-root manager. macOS defaults to user LaunchAgent; Linux `--user` remains supported.
- Service installation scope, definition ownership, runtime UID/GID, and CLI execution UID are distinct facts. Never classify service scope solely by its runtime UID.
- Keep `/tmp/runner.lock` shared across all identities and foreground/service starts; no per-user locks.
- No hidden sudo, UID switching, account takeover, endpoint fallback, broad chmod/chown, or migration inside update/--force.
- Preserve root-only binary/ancestor trust for every system installation, even when its service User= is non-root.
- New account/directory preparation and explicit migration are administrative operations. Validate all inputs and produce a bounded plan before mutations.
- Preserve configured Docker sockets/platforms, Tart homes, authentication boundaries, drain semantics, and existing app/key secrecy.
- Existing Linux root services remain a supported mode; upgrades, --force and historical rename migration preserve their runtime identity. No root-to-non-root conversion is required or scheduled.
- Update both README languages and installer guidance; prompts/agent instructions/code/commits use English. No package, account, service, or data mutation during planning.

## Review Focus

- A system service running as non-root must keep system paths, not silently adopt XDG paths below its HOME.
- Root setup can read credentials a service account cannot; validate using the actual target identity and service environment.
- Existing account names, symlinked paths, shared directories, root backup data, or unrelated user services must not be taken over.
- Duplicate/unknown scopes, sudo changing the caller, or multiple binaries must never redirect an operation to a different installation.
- Historical rename migration failure or interruption must preserve a recoverable old unit/config/permission state without killing an active job before an authorized drain; it must not change runtime identity.

## File Responsibilities

| Files | Responsibility |
| --- | --- |
| new `cmd/runner/installation.go` and tests | Resolve and describe visible installed targets and explicit scope overrides. |
| `cmd/runner/cmd_service.go`, `config_paths.go`, `service_env.go` | Service-manager discovery, new-system non-root unit, shared flags/defaults, effective runtime paths. |
| new `cmd/runner/service_identity.go` and tests | Injectable Linux account/group/path validation and bounded preparation plan; no authentication secrets in plan output. |
| `internal/layout`, `cmd/runner/logpath.go`, `trusted_path.go`, service/config tests | Explicit system/user layout and binary ownership validation independent of runtime EUID. |
| `cmd/runner/cmd_update.go`, status/logs/image commands and tests | Use the resolved installation target; preserve explicit foreground operation. |
| `cmd/runner/cmd_migrate.go`, migration tests | Preserve historical rename migration and backups; do not implicitly convert legacy root runtime. |
| `install.sh`, `README.md`, `README.zh-TW.md`, config example where required | Clear system-vs-user installation, permissions, update, and migration instructions. |

### Task 1: Resolve the installed target consistently

**Consumes:** Existing service-manager definitions/properties, explicit command flags and current UID; system and current UID's user scope only.

**Produces:** `InstallationTarget` with scope, definition owner, runtime UID/GID, absolute binary/config paths, service identifier/state; `ResolveInstallationTarget(...)` behind injected platform readers. Keep service-data parsing separate from command execution.

- [ ] Write tests for unique system/non-root target, unique user target, both scopes, unknown scopes, no installation, legacy root service, macOS legacy daemon, sudo identity changes, unit overrides, quoted paths, and symlinks. Assert explicit scope never falls back and auto mode refuses ambiguous mutations.
- [ ] Run `go test ./cmd/runner`; confirm the new target behavior fails before implementation.
- [ ] Add target discovery and `--scope auto|user|system`, preserving explicit `--user` aliases and rejecting conflicting flags. New-install defaults stay Linux system/macOS user; existing management resolves the installed target.
- [ ] Run focused race tests. Verify normal commands share one resolver and do not scan other home directories or load secret config merely to display service identity.
- [ ] Commit after implementation authorization: `feat(cli): resolve installed runner management targets`.

### Task 2: Separate service scope from runtime layout

**Consumes:** Task 1 target scope and runtime identity, existing layout and service environment helpers.

**Produces:** Explicit scope in the layout input and generated service metadata, with one effective config/state/log/diagnostic path contract. Metadata selects paths, never grants privilege.

- [ ] Write tests asserting Linux system+non-root uses `/etc/runner/config.toml`, `/var/lib/runner`, `/var/log/runner`; user scope uses XDG; macOS user keeps Library logs. Include disabled/custom log-file, service HOME, explicit config, and system root-only binary trust with non-root User=.
- [ ] Run `go test ./internal/layout ./cmd/runner`; confirm failures for the absent scope-aware behavior.
- [ ] Add scope-aware path resolution and consistent unit/runtime metadata. Keep foreground explicit-config behavior, lock visibility, and service log-path fallback intact; prevent system config from becoming service-writable.
- [ ] Run `go test -race ./internal/layout ./internal/pathtrust ./internal/lock ./cmd/runner`.
- [ ] Commit: `refactor(layout): distinguish service scope from runtime identity`.

### Task 3: Prepare Linux system installations for a non-root runtime

**Consumes:** Validated target and scope-aware paths; Linux system installation privilege only.

**Produces:** Injectable `PrepareSystemInstallation(...)` plan/apply operations for account/group/directories and unit fields. Proposed account/group is `runscaler`; unit User/Group are non-root, HOME/WorkingDirectory use `/var/lib/runner`. Manifest-prepared directories and explicit ReadWritePaths grant required writes; new non-root units omit automatic StateDirectory/LogsDirectory ownership management.

- [ ] Write tests for absent compatible account, conflicting existing account, UID 0 rejection, non-root installer refusal, root user-service refusal, root-owned binary chain, config/key read-only access, writable state/logs, 0700/0600 diagnostics, and private administrative backup directory outside service-writable state.
- [ ] Test pre-existing state/log trees containing old root-private backups, excluded files, or unknown children: preparation must refuse until the entire managed subtree is accounted for. Assert generated new units omit automatic ownership-changing directory directives, and rollback restores every manifest permission change without recursive side effects.
- [ ] Add Docker tests proving service-identity access to the exact configured socket, bounded supplementary group grants, no socket chmod/chown, no root-group grant, and refusal of another personal UID's rootless daemon. Assert rootful socket access is reported as a separate effective privilege.
- [ ] Run focused tests, then implement validation-before-mutation and non-root unit generation. Report missing Linux account utilities/manual preparation rather than installing dependencies. Do not import the installer shell's personal credentials or change an existing incompatible account.
- [ ] Ensure --force and the historical root-command migrate retain an existing Linux root runtime without prompting an identity conversion; new system installations use the dedicated non-root account.
- [ ] Run `go test -race ./cmd/runner ./internal/config ./internal/layout ./internal/provider`; verify all system/user/macOS fixtures retain their intended semantics.
- [ ] Commit: `feat(service): run new Linux system services as a dedicated user`.

### Task 4: Align management, update, and maintenance commands

**Consumes:** Task 1 installed target, Tasks 2–3 runtime/path contract.

**Produces:** Shared target selection in status/logs/service commands/update and image command integration once available; explicit `update --binary-path PATH` for a verified binary-only operation.

- [ ] Write tests with distinct invoking/service binaries and configs. Assert update selects the service binary, system permission failure is reported before download, user update works without sudo, sudo hints use quoted absolute paths, and the non-selected binary is unchanged.
- [ ] Test macOS root-owned shared binary update separately from user service operation; no root LaunchAgent, implicit UID switch, or restart. Test config/scope/binary override conflicts and unreadable private config/logs without permission broadening.
- [ ] Test a unit with Docker access available only through unit SupplementaryGroups: plain sudo -u lacks access, but generated systemd-run transient-service properties reproduce the selected UID/GID, extra groups, HOME, working directory, scope/paths, and necessary credential sources. Include missing features, dollar-containing argv, secret environment values, and command cancellation. Assert no secret is expanded into argv/output, no arbitrary unit actions/directory ownership directives are copied, and no permanent group mutation or user-environment substitution occurs. Preserve the machine lock and stopped-manager requirement.
- [ ] Implement preflight target/access validation and precise operator commands. Read-only service information may be available without root; inaccessible secrets do not get copied into public target metadata.
- [ ] Run `go test -race ./cmd/runner ./internal/versioncheck ./internal/lock`.
- [ ] Commit: `feat(cli): bind runner operations to the installed service`.

### Former Task 5: Withdrawn — preserve existing Linux root runtimes

The user clarified that existing root services should remain root. Identity
conversion is not an unfinished upgrade requirement and is not a planned next
increment. Keep regression coverage proving update, --force and the historical
rename migration preserve the existing runtime. Reconsider conversion only if
the user explicitly requests a separate feature in the future.

## Documentation and Authorized Live Gates

- [ ] Update installer/help and both READMEs to distinguish a binary download from service scope: Linux default system uses root-owned binary, Linux --user and macOS use user-owned defaults. Do not silently move an existing binary.
- [ ] Verify a new Linux system service starts after reboot with a non-root UID and system paths, config/key read-only, and state/logs writable; exercise one real Docker job and graceful drain.
- [ ] Verify Linux user installation/update and a same-UID rootless Docker job, including cache/helper/socket/resource-limit behavior. Check optional linger separately, without enabling it implicitly.
- [ ] Verify macOS user installation/update, one Tart job and drain; root launch paths remain rejected.
- [ ] On an explicitly authorized fixture host, verify an existing Linux root service retains its UID, data ownership and provider access through upgrade and --force. No remote service change is authorized merely by this planning document.

## Draft Self-Review

The deployment modes, scope/UID separation, administrative updates, shared target discovery and rootful/rootless daemon distinction map to the tasks above. Existing Linux root runtimes remain supported. Implementation and automated verification status are recorded above and in the worktree ledger; live verification remains outstanding.
