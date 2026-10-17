# Quark Drive Mount Client

This project provides a Windows desktop client that exposes one Quark cloud-drive account as a local drive letter. The glossary names the user-facing and domain concepts shared by the client, mount engine, and Quark adapter.

## Account And Authentication

**Quark account**:
The single Quark cloud-drive identity managed by the first release.
_Avoid_: storage, remote, profile

**Cookie credential**:
The authenticated Quark web-session data imported by the user and protected locally by the client.
_Avoid_: password, API key, login token

**Session refresh**:
The process of accepting refreshed Quark session fields while the account remains authenticated.
_Avoid_: re-login, token rotation

**Credential validation**:
The check that a candidate Cookie credential can authenticate Quark configuration and list the selected root before it becomes active.
_Avoid_: syntax check, login probe

**Reauthentication**:
The flow that replaces a Quark Cookie credential after the current credential can no longer authenticate, while preserving the mount profile and dirty cache.
_Avoid_: account reset, remount configuration

## Mounting

**Mount profile**:
The saved user choice that binds the Quark account to a drive letter and a root path.
_Avoid_: account, mapping, shortcut

**Configuration version**:
The schema identifier used to migrate the saved non-sensitive application configuration safely.
_Avoid_: app version, protocol version

**Capability snapshot**:
The latest non-sensitive record of the operations the adapter declares and the last probe result for the active Quark credential.
_Avoid_: permission cache, live guarantee

**Error class**:
The stable, non-sensitive category attached to an adapter failure so the WebDAV endpoint, supervisor, tray UI, and logs agree on its meaning.
_Avoid_: provider message, raw HTTP error

**Write diagnostic**:
The user-triggered small-file write and cleanup check used to verify mutating Quark operations without running automatically at startup.
_Avoid_: background sync, health check

**Drive mount**:
The live Windows filesystem view that presents the configured Quark root at a drive letter.
_Avoid_: sync, mirror, downloaded copy

**Mount engine**:
The process or embedded component that translates Windows filesystem requests into Quark file operations.
_Avoid_: WebDAV server, synchronizer

**Operation capability**:
The currently supported set of file operations for the account and mount, such as list, read, write, move, rename, or delete.
_Avoid_: permission, feature flag

**Quark adapter process**:
The standalone process that contains the Quark account logic and exposes that account through the internal WebDAV endpoint.
_Avoid_: AList service, cloud service

**Session store**:
The adapter-owned concurrency-safe holder of the current Quark Cookie snapshot and refreshed session fields.
_Avoid_: credential vault, mount profile

**Quark object identity**:
The provider FID used as the stable identity of a Quark file or directory; a WebDAV path resolves to this identity for an operation.
_Avoid_: local inode, path identity

**Internal WebDAV endpoint**:
The authenticated loopback-only WebDAV endpoint consumed by the mount engine; it is an implementation interface, not a user-facing sharing service.
_Avoid_: public WebDAV server, WebDAV entrance

**Temporary runtime configuration**:
The ACL-restricted, short-lived rclone WebDAV configuration generated for one mount session and deleted after the mount stops.
_Avoid_: user rclone config, persistent remote profile

**Mount supervisor**:
The tray-owned lifecycle controller that starts, observes, stops, and recovers the Quark adapter process and mount engine process as one mount session.
_Avoid_: Windows service, scheduler

**Recovery state**:
The current status of starting, retrying, mounted, paused, failed, or intentionally disabled automatic mount recovery.
_Avoid_: connection status, health check

**NeedsReauth**:
A recovery state in which the Quark credential is known to be unauthenticated and user input is required before the drive mount can resume.
_Avoid_: network failure, generic mount failure

**DriveConflict**:
A recovery state in which the configured drive letter is already occupied and automatic mounting is paused until the conflict is resolved.
_Avoid_: mount failure, network conflict

**WinFspRequired**:
A recovery state in which the matching WinFsp runtime is missing, unusable, or incompatible with the mount process architecture.
_Avoid_: generic dependency error, driver offline

**Runtime bundle**:
The application-distributed, architecture-matched rclone executable and its integrity metadata used for a mount session.
_Avoid_: global rclone install, user PATH dependency

**Upgrade staging**:
The temporary installation area where a new application/runtime version is checked before it replaces the active version.
_Avoid_: cache directory, runtime directory

**Release gate**:
A condition that must pass before a build can be published to users.
_Avoid_: smoke check, best-effort check

**Compatibility matrix**:
The tested combination of Windows version, process architecture, WinFsp runtime, network conditions, and mount lifecycle events.
_Avoid_: supported list, environment note

**Dirty cache**:
A local file version that has been written by the user but has not yet been confirmed as uploaded to Quark.
_Avoid_: offline copy, synchronized file, backup

**Writeback ledger**:
The per-mount record of dirty-cache writebacks the provider refused, carrying the file path, stable error category, retry count and next retry time.
_Avoid_: error log, upload queue, retry queue

**Instance cache directory**:
The VFS cache directory one mount instance owns exclusively, derived from the mount point and kept until the user cleans it.
_Avoid_: shared cache, temp folder, cache directory (unqualified)

**Main panel**:
The tray's loopback web page carrying the status, diagnostics and setup views; the single surface for wizards and recovery flows.
_Avoid_: dashboard, control panel, settings window

**Recovery timeline**:
The mount's state transitions rendered oldest-first in the diagnostics view, capped in memory.
_Avoid_: event log, history list, audit trail

**Panel token**:
The per-process random value every state-changing panel request must present, isolating the mount from other local processes.
_Avoid_: password, session id, API key

**Single-instance guard**:
The per-session named mutex that stops a second tray instance of any version from starting, so one drive letter and one cache directory never have two owners.
_Avoid_: lock file, watchdog, duplicate check

**Quarantine directory**:
The timestamped drive folder the dedicated account's acceptance lifecycle creates, exercises and deletes; everything the release verification writes stays inside it.
_Avoid_: test folder, temp dir, scratch space
