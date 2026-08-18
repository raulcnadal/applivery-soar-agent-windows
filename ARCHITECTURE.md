# Architecture Guide

This is the developer-facing companion to [README.md](README.md) (what this agent reports, every Managed Configuration registry value, deployment steps, troubleshooting). This document explains how the codebase itself is put together — for an engineer picking this repo up for the first time.

This repo builds **three separate Windows executables from one Go module**, installed together by a single MSI, no shared code with the macOS Agent repo (different language, same reporting contract — see the SOAR backend repo's own `ARCHITECTURE.md` §9 for that shared half):

1. **`Applivery-SOAR-Agent.exe`** (root `main.go` + the flat `*_windows.go` files) — the actual reporting engine, a Windows Service (`AppliverySOARAgent`) running as `LocalSystem`.
2. **`Applivery-SOAR-Watchdog.exe`** (`watchdog/main.go`) — a second, minimal Windows Service (`AppliverySOARWatchdog`) whose only job is restarting the agent service if it's found stopped.
3. **`Applivery-SOAR-Tray.exe`** (`tray/`) — a small, unprivileged, per-user-session process that shows a notification-area icon and status card. Deliberately *not* a service (Session 0 has no desktop access since Vista's session isolation — a service can never show UI in an interactive session), so it's launched instead via a logon-triggered Scheduled Task the MSI installs.

## 1. The agent service (repo root, `package main`)

`main.go` handles both service and interactive-debug entry (`svc.IsAnInteractiveSession()`), sets up rotating file logging to `%ProgramData%\Applivery\SOAR\agent.log` (a Windows Service has no console — `setupFileLogging`'s own doc comment has the story of a real bug where a broken `os.Stdout` silently blackholed every log line via `io.MultiWriter` before this was fixed to make the file, not stdout, the writer that must succeed), and starts both `runAgentLoop` and the mutual-watchdog poll (`svcwatch.Monitor`, §3) as goroutines.

- **`registry_windows.go`** — `Config`, read from `HKLM\SOFTWARE\Policies\Applivery\SOAR` (an MDM-pushed registry policy, README's full value table) — the Windows equivalent of the macOS agent's JSON preference file, using whatever native Managed Configuration mechanism each OS actually has.
- **`telemetry_windows.go`** — `runAgentLoop`: the same two-ticker shape as the macOS agent (a `Config.IntervalSec`-driven report cycle, `gatherAndReport`, plus a tight poll for the tray's trigger flags and for a SOAR-pushed `remoteIntervalSec` override — see §3), Go being the one part of this stack that's genuinely near-identical across both agent repos despite zero shared code.
- **`wmi_windows.go`** — BitLocker (`Win32_EncryptableVolume.ProtectionStatus` via WMI) and OS build number (`Win32_OperatingSystem`).
- **`registry_windows.go`** (Defender/Firewall reads) and the rest of the security-signal collection largely goes through `golang.org/x/sys/windows/registry` and WMI rather than shelling out, unlike the macOS agent's `fdesetup`/`socketfilterfw` CLI calls — whichever's the native Windows API for a given signal.
- **`apps_windows.go`** — installed-application inventory, preferring `winget list` (matching `report-installed-apps.ps1`'s own approach) so a self-report and the winget-sourced entries in App Lists compliance conditions match on the same `PackageIdentifier` precisely.
- **`customchecks_windows.go`** — the five Custom Device Check types, Windows-appropriate implementations (registry value, SCM service status, installed-app-and-version, process running, arbitrary PowerShell) of the same JSON contract the macOS agent's `customchecks_macos.go` implements independently.
- **`eventwatch_windows.go`** + **`etw_windows.go`** — **two** event-driven detection mechanisms (the macOS agent only needs one, `fsnotify`, since Darwin's kqueue covers its watch types): a registry-key watcher (`eventwatch_windows.go`) and an ETW (Event Tracing for Windows) provider watcher (`etw_windows.go`, via `github.com/0xrawsec/golang-etw`) for signals a registry watch can't see. Both are additive "fast lanes" alongside the normal per-cycle poll — see `backend/docs/event-driven-agent-detection-roadmap.md` in the SOAR repo for why that's a deliberate, not accidental, design choice.
- **`mtls_windows.go`** — mTLS agent identity: local keypair generation, one-time CSR registration via the Managed-Configuration-delivered Global Bootstrap Token, self-renewing thereafter. File-based/Certificate-Store simplification mirrored 1:1 by the macOS agent's own `mtls_macos.go` — see that repo's `ARCHITECTURE.md` §1 for the shared rationale, or `backend/docs/mtls-agent-auth-roadmap.md` (SOAR repo) for the full design.
- **`status_windows.go`** — thin client for `GET /api/device-data/agent-status`, feeding `internal/agentstatus` (§3).

## 2. `internal/` packages

Factored out specifically because more than one of this repo's three binaries needs them — the opposite of the macOS Agent repo, which has no `internal/` split at all because it only ever had one binary needing daemon-side logic.

- **`internal/agentstatus`** — `AgentStatusPolicy`/`AgentStatusViolation`/`AgentStatusCompliance`/`AgentStatusDevice`/`AgentStatusResponse`, shared between the agent service (which writes `status.json`) and the tray (which reads it) so the two can never drift apart on field names the way two hand-copied struct definitions eventually would. Mirrors the backend's `GET /api/device-data/agent-status` response shape closely enough for `encoding/json` to populate directly — kept in sync by hand across the Go/TypeScript language boundary, not code-generated; a field the backend adds later is silently dropped here until this struct catches up, nothing panics.
- **`internal/svcwatch`** — the mutual-watchdog primitive (`svcwatch.go`: `EnsureRunning`, opens a named service via the Service Control Manager and starts it if `SERVICE_STOPPED`) plus tray supervision (`tray.go`: `EnsureTrayRunning`, a `CreateToolhelp32Snapshot` process-list scan since the tray isn't a service SCM can query — re-launches it if it's gone missing mid-session, closing the gap in the Scheduled Task's own "only fires once per logon" limitation). **Anti-tampering model**: `AppliverySOARAgent` and `AppliverySOARWatchdog` each watch the other and restart it within one poll interval if a local admin stops it via `sc stop`/`services.msc`/Task Manager (Windows' own Service Recovery Actions only fire on a crash, never a clean SCM stop, so they don't help against deliberate tampering). This is an explicitly-documented deterrent, not a kernel-mode guarantee — stopping *both* within the same short window, or removing the services entirely, still defeats it, the same tier of protection real-world EDR/MDM agents commonly ship.
- **`internal/agentlog`** — shared rotating-file-logger setup for the watchdog and tray (the main agent keeps its own original copy of the same logic in `main.go` untouched, to avoid any risk of regressing its already-verified behavior while refactoring).

## 3. IPC contract — `%ProgramData%\Applivery\SOAR\`

Same design as the macOS Agent's shared-directory contract (that repo's `ARCHITECTURE.md` §3), adapted to Windows primitives instead of Unix file permissions:

- **`agent.log`** — rotating log file (§1), also the shared base path `internal/agentlog` centralizes.
- **`status.json`** — written by the agent service after every report cycle; read by the tray on every card open plus its own 60s refresh timer, so it's never more than one cycle stale.
- Trigger flag file(s) — written by the tray's "Force report"/"Force evaluate compliance" buttons (`triggerForceReport`/`triggerForceEvaluate`, `tray/main.go`), polled and consumed by the agent service's own tight ticker in `telemetry_windows.go`. The tray never calls the backend directly for these — it has no HTTP client and no access to the Managed Configuration secret at all (`tray/main.go`'s own doc comment), strictly by design: only the authenticated service acts, the tray only signals.

## 4. The tray helper (`tray/`)

Built entirely on **raw syscalls** against `user32.dll`/`gdi32.dll`/`shell32.dll` via the standard library's `syscall` package — no GUI toolkit, no WebView2 dependency. Deliberate: a new dependency's `go.sum` entries need network access to resolve that this project's offline development sandbox doesn't reliably have, and hand-rolled Win32 calls are something CI (`build.yml`, §6) can actually compile-verify the same way it verifies everything else here.

- **`main.go`** — notification-area icon registration (`Shell_NotifyIconW`), the message loop, and the two trigger-writing functions.
- **`card.go`** — the status card: a second borderless top-level window (`WS_POPUP`, not a dialog resource), custom-painted to match the SOAR web dashboard's own Workspace Profile modal styling (banner logo, device name/workspace slug, status pills, boxed sections) so the native tray experience visually reads as the same product as the web app. Always fully opaque — a translucent Acrylic backdrop (the Windows equivalent of the macOS menu bar app's `NSVisualEffectView`, via `DwmExtendFrameIntoClientArea` + the undocumented `SetWindowCompositionAttribute`) was tried and reverted after real-device testing showed the blur/tint bleeding across already-opaque content instead of staying confined to the empty background, washing out light-theme text and haloing the banner bitmap. Opened anchored above and right-aligned to the tray icon itself (`Shell_NotifyIconGetRect`, `cardPosition`), matching the macOS panel's own icon-anchored positioning, rather than dead-centered on screen.
- **`gdi.go`** — the raw GDI drawing/font-loading plumbing `card.go` calls into, split out purely to keep `card.go`'s actual card logic from being crowded out by `proc`-binding boilerplate.
- **`fonts.go`** + **`fonts/`** — three embedded static Outfit TTF weights, loaded via `AddFontMemResourceEx` at runtime (falling back to Segoe UI if that ever fails — the one card behavior this repo's offline build can't verify ahead of time, no local Windows/GDI to actually exercise it against). Same three weights the macOS menu bar app bundles, so both cards read identically.
- **`icons/`** — `tray_light.ico`/`tray_dark.ico` (the notification-area icon itself — AppKit auto-inverts the macOS status item's SF Symbol for light/dark menu bars "for free"; Windows has no equivalent, so this repo ships both variants explicitly) and `banner_light.bmp`/`banner_dark.bmp` (rasterized from `icons/applivery-bp-login.svg`, same `cairosvg` + ImageMagick `-trim` pipeline the macOS agent's own banner asset uses).

There is deliberately no "open dashboard" link or way to exit/stop the tray or agent from this process — it runs on end-user devices, not admin machines (`tray/main.go`'s doc comment, `agent.wxs`'s tamper-resistance design).

## 5. Installer (`agent.wxs`, WiX Toolset v4)

One MSI (`Package.Scope="perMachine"`) installs all three components:

- **Component ordering is load-bearing, not cosmetic.** `WatchdogExecutable` is declared *before* `AgentExecutable` in the `.wxs` — WiX schedules each component's `ServiceControl` Stop/Start rows into the standard `StopServices`/`StartServices` actions in declaration order, so install/upgrade/uninstall always runs "stop watchdog, stop agent, replace files, start agent, start watchdog." Reversing that order would let the still-running watchdog restart the agent mid-upgrade, re-locking a binary MSI is actively trying to overwrite — the mutual-watchdog design (§2) actively fighting its own installer if this weren't sequenced correctly.
- **`AllowSameVersionUpgrades="yes"`** — this repo's CI never bumps `Package Version` past `1.0.0.0`; every push publishes new binary content under the same version string (the same "rolling latest build, content-addressed not version-addressed" model the SOAR backend's own Docker `:latest` tag uses). Without this attribute, Windows Installer treats a same-version reinstall as a no-op and silently keeps the old binary running — this is what makes a UEM re-push of the same MSI actually replace the files every time.
- **Tray Scheduled Task** — registered with an `ONLOGON` trigger (not a service — see §"Overview" above), so it starts once for whichever user logs on. `internal/svcwatch/tray.go`'s process-snapshot supervision (§2) covers the gap where that only fires once per logon, not on a mid-session crash/kill.
- **`versioninfo/{agent,tray,watchdog}.json`** — per-exe `FixedFileInfo`/`StringFileInfo` (FileVersion, CompanyName, ProductName, ...), compiled into a `resource.syso` next to each binary's own `main.go` by `goversioninfo` at build time (§6) so each `.exe` has a real embedded VERSIONINFO resource instead of Explorer's Properties dialog showing "0.0.0.0"/no publisher.

## 6. Build & CI (`.github/workflows/build.yml`)

Runs on `windows-latest` (the only place in this project's tooling with a real WiX/MSI toolchain):

1. `go mod tidy` — resolves `go.sum` for any dependency added since the last CI run against this runner's real network access (this repo's own development sandbox has no reliable route to the Go module proxy).
2. `goversioninfo` (continue-on-error — version metadata is cosmetic, never worth failing the build over) turns `versioninfo/*.json` into a `.syso` per binary.
3. Two full builds — **x64** and **ARM64** — of all three Go binaries plus a `wix build` of `agent.wxs` for each, producing two separate MSIs (`Applivery-SOAR-Agent-x64.msi` / `-arm64.msi`; unlike the macOS Agent, which `lipo`s a single universal binary, Windows has no equivalent so this repo ships two installers instead).
4. On push to `main` only: both MSIs are POSTed to the SOAR backend's zero-config ingest endpoint (`POST /api/internal/agent-builds/windows`, `X-Agent-Arch: amd64|arm64`, `SOAR_AGENT_BUILD_SECRET` shared-secret header — see the SOAR repo's `ARCHITECTURE.md` §9.2) so a customer can download either from **Settings → Applivery SOAR Agent** with zero GitHub token, and published as this repo's rolling `latest` GitHub Release (same tag reused every push) for the secondary GitHub-token-proxied download path.

Every push (including PRs) runs the full build for compile verification; the publish steps only run on push to `main`.

## 7. Testing

Same story as the macOS Agent repo: no local Windows build machine in this project's own tooling, so `windows-latest` CI (§6) is the sole compile verification for all three binaries, and real end-to-end behavior (the mutual-watchdog restart behavior, the tray's Win32 rendering, the MSI's service-ordering-dependent upgrade path, actual registry-policy reads) has historically been verified against a real Windows device, not an automated test suite in this repo. Treat a change here as unverified until CI builds it and it's actually exercised on a device.

## Further reading

[README.md](README.md) — what this agent reports (the full Self-Reported Attribute list), every Managed Configuration registry value, deployment steps for both the zero-config and GitHub-token download paths, the mTLS Agent Authentication rollout story from this agent's side, and Troubleshooting.
