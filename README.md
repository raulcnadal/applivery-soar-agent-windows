# Applivery SOAR Agent — Windows

A lightweight, native Go background service for Windows devices. It collects
security posture, device telemetry, and (optionally) software inventory and
custom admin-defined checks, then reports them to an
[Applivery SOAR](https://github.com/raulcnadal/applivery-soar) instance,
where they become available as **Compliance Policy** conditions and
Overview/Devices telemetry.

The agent is compiled into three native 64-bit or ARM64 executables and
packaged as a **WiX v4 MSI installer** (`Applivery-SOAR-Agent-amd64.msi` /
`-arm64.msi`):

* `Applivery-SOAR-Agent.exe` — the `AppliverySOARAgent` Windows Service
  (`LocalSystem`) described throughout this README: telemetry, reporting,
  Custom Device Checks.
* `Applivery-SOAR-Watchdog.exe` — the `AppliverySOARWatchdog` Windows
  Service. Exists primarily to make the agent harder to switch off (see
  *Tamper resistance* below) — it also polls for the tray helper process
  below and silently re-launches it if it's gone missing mid-session.
* `Applivery-SOAR-Tray.exe` — an unprivileged per-user tray icon, launched
  by a Scheduled Task at logon (not a service — see *Tray icon* below).

---

## Getting the binary

You don't need to build this yourself, and you don't need a GitHub token —
the compiled MSI is downloadable straight from your SOAR instance:

**Settings → Applivery SOAR Agent**, click **Download**
next to Windows (x64) or Windows (ARM64). This app's own CI publishes a
fresh MSI for *both* architectures there on every push to `main`, mirrored
into the SOAR backend itself (no GitHub authentication needed, same as
pulling a public Docker image). The same Settings page has a **Publish to
Applivery** button next to each — x64 and ARM64 are independent, each its
own Applivery application — that uploads the binary straight into your
Applivery organization, so it can be assigned to Policies like any other
managed app.

This repo's raw GitHub Releases remain available as a fallback via the
optional GitHub-token path further down the same Settings panel, for anyone
who already configured it or would rather not rely on the SOAR backend's own
mirror.

---

## Architecture

* **Windows Service:** Runs silently under `LocalSystem`, which gives it the
  privileges needed to query BitLocker, the Registry, and WMI, and keeps it
  running independently of any user session.
* **Managed Configuration:** No endpoint or secret is baked into the binary.
  At startup (and the agent does not need a restart to notice a value that
  changes) it reads `HKLM\SOFTWARE\Policies\Applivery\SOAR`, which any UEM —
  Applivery itself, Intune, etc. — can push as a registry-backed
  configuration profile.
* **Reporting loop:** Wakes on a configurable timer (`IntervalSec`, default
  1 hour — 3600s), gathers telemetry, and POSTs it with retry + backoff. A
  second, much shorter timer (~2s) checks for a force-report/force-evaluate
  trigger file dropped by the tray's action buttons (see "Tray icon" below)
  and, if present, runs an extra out-of-cycle pass immediately.
* **Custom Device Checks:** Once per cycle, before reporting, the agent polls
  the backend for whatever checks an admin has defined for Windows in
  **Settings → Custom Device Checks**, runs each one locally, and includes
  the results in the same report — no separate call, no local state kept
  between cycles. A check created or edited in the dashboard takes effect on
  this device's very next report.

---

## Tamper resistance

A local administrator can always stop a Windows Service outright (`sc stop`,
`services.msc`, Task Manager's Services tab) — Service Recovery Actions (the
built-in "restart on failure" configured per service) only fire on a crash,
never on a clean stop, so they don't help against someone deliberately
switching the agent off. To raise the bar against that, `AppliverySOARAgent`
and `AppliverySOARWatchdog` watch **each other**: every 30 seconds (after an
initial 60s grace period so a normal boot doesn't race both services
starting up against each other), each asks the Service Control Manager
whether its partner is running, and restarts it if it's found stopped
(`internal/svcwatch/svcwatch.go`). Stopping either service alone gets it
silently restarted by its partner within about 30 seconds; defeating this
for real requires stopping (or deleting) both within that same short window.

This is a deterrent, not a kernel-mode guarantee — same tier of protection
commonly shipped by real-world EDR/MDM agents, not a claim that a
determined local administrator can never disable it. `agent.wxs` stops the
watchdog before the agent on every install/upgrade/uninstall specifically so
this mutual-restart logic never fights the installer itself (see the
comment on the `WatchdogExecutable` component).

## Tray icon

`Applivery-SOAR-Tray.exe` gives a logged-in user a visible, at-a-glance
signal that device management is active, without needing to open
`services.msc`. It's launched by a Scheduled Task (`Applivery SOAR Tray`,
registered by `agent.wxs` via `schtasks.exe` at install time, scoped to the
built-in `Users` group so it fires for any interactive logon) rather than
being a service itself — Windows Services run in Session 0, which has had
no desktop access since Vista's session isolation, so a service can never
show tray UI directly.

* **The icon itself** is Solar's `shield-check` glyph (matching the icon set
  used throughout the SOAR web app), swapping automatically between a
  dark-glyph and light-glyph variant to match the taskbar's own light/dark
  setting (`HKCU\...\Themes\Personalize\SystemUsesLightTheme`), checked on
  `WM_SETTINGCHANGE` and on a 60s backstop timer. The process opts into
  Per-Monitor-v2 DPI awareness (`SetProcessDpiAwarenessContext`) on startup
  so the icon renders crisp — not blurred/upscaled — on scaled displays.
* **Click** (left or right — both open the same view) shows a status card: a
  BlueSky-styled window (brand blue, rounded corners) rather than a native
  popup menu, opened centered on screen but freely drag-movable afterward
  (click anywhere on the card except the close button/action buttons and
  drag — standard Win32 window-move behavior, not a custom drag
  implementation). Text renders in the product's actual brand typeface —
  Outfit, at Regular/SemiBold/Bold weights — via 3 static TTFs embedded
  into the binary and privately registered at runtime with
  `AddFontMemResourceEx` (`tray/fonts.go`; nothing written to disk, nothing
  visible to other processes), falling back to the system Segoe UI font
  wholesale if that registration ever fails rather than mixing typefaces.
  The header shows the Applivery SOAR banner logo top-left, then this
  device's name (as reported by Applivery) with a Status pill and the
  compliance-state pill (Compliant / N issues / Unavailable) at the right
  of that same line. Below that: what's currently being reported (last
  report time/result, BitLocker/Firewall status, whether app inventory is
  included), and this device's Compliance Policy status — risk score and
  tier, and the applicable policies for this platform with a per-policy
  OK/Violation pill. The footer reads "Managed by {workspace slug}"
  followed by the card's own last-updated time. The card sizes itself to
  whatever it needs to show (long device/policy names grow it wider rather
  than truncating, up to 90% of the screen width) rather than a fixed size.
  There is deliberately no link to the SOAR dashboard and no way to exit
  the tray from here — this agent runs on end-user devices, not admin
  machines, and the tray is part of the same tamper-resistance story as the
  watchdog service above. Dismiss with the close button, `Esc`, or by
  clicking elsewhere.
* **"Force report" / "Force evaluate compliance" buttons**, right under the
  header, let the person at this device kick off an out-of-cycle report or
  compliance evaluation instead of waiting for the next scheduled tick
  (report interval, or the backend's own 60s evaluation scheduler). Neither
  button calls the backend from the tray process itself — the tray has no
  HTTP client or `ReportSecret` of its own (see below) — clicking one just
  drops an empty marker file under `%ProgramData%\Applivery\SOAR\` that the
  main service polls for every ~2 seconds and actions (an immediate
  `gatherAndReport()` cycle, or a `POST /api/device-data/evaluate-now`
  request). A balloon confirms the request was queued; `agent.log` has the
  actual outcome (e.g. if no Automation Credential is configured for the
  workspace yet).
* **Notifications.** The tray also raises a Windows balloon/Action Center
  notification when this device's compliance state actually changes state
  while the tray is running: one or more policy violations newly detected,
  or a full recovery back to compliant. It only fires on that transition
  (not on every poll), and only for changes observed after the tray starts
  — it doesn't notify about whatever state the device happened to already
  be in at startup.
* **It never talks to the backend directly.** The tray process has no
  registry access to the `ReportSecret` and no HTTP client of its own — it
  only reads `%ProgramData%\Applivery\SOAR\status.json` (written by the
  agent service after every report cycle) and, for the two force-action
  buttons, writes an empty trigger file the service polls for. This keeps
  the tray simple and guarantees the status it shows always reflects exactly
  what the already-authenticated service actually last reported.

---

## Configuration Reference (Managed Configuration)

All values live under `HKLM\SOFTWARE\Policies\Applivery\SOAR`. There is no
compiled-in default for `WorkspaceSlug` or `ReportSecret` — until both are
set, the agent logs a warning each cycle and reports nothing.

| Registry Value | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `BaseURL` | String | `https://soar.mi-labs.es` | Base URL used for reporting (report, report-apps, custom-checks, event-watches, event-notify, agent-status) AND certificate renewal. Once a workspace uses mTLS, this must point at the dedicated agent subdomain (Settings → mTLS Agent Authentication → Reverse Proxy Configuration) — renewal always requires a valid client certificate, so it must go through that vhost. |
| `RegisterURL` | String | *(none — optional, falls back to `BaseURL`)* | Base URL used ONLY for the one-time `/api/device-mtls/register` call. `/register` never presents a client cert (the bootstrap token is the credential), so it doesn't need the mTLS vhost's health at all — setting this to the ordinary dashboard domain decouples first-time enrollment from whether that vhost happens to be up. Leave unset for the historical single-URL behavior. |
| `WorkspaceSlug` | String | *(none — required)* | Your workspace identifier. |
| `ReportSecret` | String | *(none — optional)* | Device-report webhook secret (Settings → Applivery SOAR Agent → Generate webhook secret). Either this or `BootstrapToken` must be set — an mTLS-only deployment (only `BootstrapToken` set) is fully supported. |
| `BootstrapToken` | String | *(none — optional)* | The workspace's Global Bootstrap Token (Settings → mTLS Agent Authentication → Generate). The SAME value is pushed to every device in the fleet — see [mTLS Agent Authentication](#mtls-agent-authentication) below. Safe to leave unset if your workspace hasn't enabled mTLS yet. |
| `IntervalSec` | DWORD | `3600` | Reporting interval in seconds (values under 30 fall back to the default). |
| `ReportBitLocker` | DWORD (1/0) | `1` | Include BitLocker disk-encryption status. |
| `ReportFirewall` | DWORD (1/0) | `1` | Include Windows Firewall status. |
| `ReportApps` | DWORD (1/0) | `0` | Include the full installed-application inventory. |

Settings → Applivery SOAR Agent generates a ready-to-import `.reg`/`.ps1` file
with all of these pre-filled for your workspace (including `BootstrapToken`,
if one is configured) — you shouldn't need to type any of this by hand.

---

## mTLS Agent Authentication

The agent can authenticate to SOAR with a per-device client certificate
instead of the shared `ReportSecret`. This is opt-in per workspace; nothing
changes for a device that never receives a `BootstrapToken`.

Registration uses a single **Global Bootstrap Token**: one value, the SAME
on every device in the fleet, pushed via one Managed Configuration policy —
not a per-device or one-time credential. A device proves it's allowed to
register with that token PLUS a live check (done server-side) that its own
serial number is currently a known, enrolled device in this workspace's
Applivery UEM fleet. Only devices Applivery already knows about can ever
register.

1. **First run with `BootstrapToken` set:** the agent generates an ECDSA
   P-256 keypair locally (the private key never leaves the device), builds a
   CSR, and registers with the backend over plain HTTPS using the token —
   against `RegisterURL` if set, otherwise `BaseURL`.
   The backend validates the token, checks the device's serial number
   against Applivery's live fleet, and — if both check out — issues a
   certificate immediately (no admin approval step; a bootstrap token is
   unattended by design). The issued certificate + key are stored under
   `%ProgramData%\Applivery\SOAR\mtls\` (ACL-locked to SYSTEM and local
   Administrators only — this is a file-based keystore, not the real
   Windows Certificate Store, a deliberate simplification rather than an
   oversight). A device that already has an active certificate can never be silently
   re-registered this way — the backend rejects it — so leaving
   `BootstrapToken` in place after enrollment is harmless.
2. **Every report cycle afterward:** if a valid certificate is loaded, ALL
   requests to the backend (reports, custom-checks poll, event-watches poll,
   agent-status, force-evaluate) present it via mTLS instead of sending
   `X-Device-Report-Secret` — the two auth modes are never mixed on the same
   request.
3. **Renewal is automatic and silent:** once less than a third of the
   certificate's total validity window remains, the agent generates a fresh
   keypair+CSR and renews using its current (still-valid) certificate to
   authenticate the renewal call — no bootstrap token is ever needed again
   after the first successful registration.
4. **If registration/renewal fails** (backend unreachable, no CA configured
   yet, token wrong, serial number not yet visible to Applivery), the agent
   just keeps using whatever auth it already has (the legacy secret, or its
   current not-yet-expired certificate) and retries on the next report
   cycle — never blocks or fails a report because of this.

**Reverse proxy**: the proxy in front of the backend must terminate the mTLS
handshake and forward the verified client identity via headers — Settings →
mTLS Agent Authentication shows the exact nginx/NPM config (and the
equivalent for Traefik/Caddy/HAProxy) plus whether the internal proxy secret
is currently configured on this backend.

**macOS parity:** the macOS Agent repo now implements the identical client
logic (same endpoints, same Global Bootstrap Token model, same renewal
window) — see that repo's own README §"mTLS Agent Authentication" for its
one platform-specific difference (a root-owned Unix-permissions keystore
instead of `icacls`).

---

## Telemetry & Data Collection

Everything is read natively via Win32/WMI/Registry APIs — no PowerShell
scripts are shelled out to for the built-in telemetry (Custom Device Checks'
`command` checker type is the one deliberate exception; see below):

1. **Device identity** — hardware serial number via WMI `Win32_BIOS`.
2. **OS build** — `SOFTWARE\Microsoft\Windows NT\CurrentVersion`.
3. **OS edition** (Pro/Enterprise/Enterprise LTSC/Education/...) —
   `SOFTWARE\Microsoft\Windows NT\CurrentVersion\EditionID`, mapped to a
   human-readable label. Applivery's own device inventory only ever reports
   the raw OS build number, never which edition is installed, so this is
   agent-only data — the backend pairs it with its own build-to-feature-name
   lookup (e.g. build `28000` → "Windows 11, version 26H1") to show a full
   "Windows 11, version 26H1 · Pro" line in the device's Overview tab.
4. **BitLocker status** — `root\CIMv2\Security\MicrosoftVolumeEncryption`.
5. **Firewall status** — `SYSTEM\CurrentControlSet\Services\SharedAccess\Parameters\FirewallPolicy\StandardProfile`.
6. **Installed software** (when `ReportApps=1`) — `winget list` when
   available (preferred: its `Id` matches App Lists' Winget search source
   exactly), tagged `origin: "winget"`; otherwise 64-bit, 32-bit
   (`WOW6432Node`), and per-user `Uninstall` registry keys, deduplicated into
   clean name/version pairs and tagged `origin: "msi"` (only reached when
   winget itself isn't invokable, e.g. running as LocalSystem with no App
   Installer package present). Either way, AppX/Store packages (Calculator,
   Store-installed apps, and other UWP apps — invisible to both winget and
   the registry) are enumerated separately via PowerShell
   `Get-AppxPackage -AllUsers` and appended, tagged `origin: "store"`. For
   each AppX package, the reported name is resolved to its real,
   human-friendly display name (e.g. "Angry Birds 2") by reading
   `AppxManifest.xml` directly off disk from the package's own
   `InstallLocation`, plus a P/Invoke to `SHLoadIndirectString` for the
   (fairly common) case where the manifest's `DisplayName` is an
   `ms-resource:`-indirect reference rather than a literal string —
   `Get-AppxPackage`'s own `Name` property is just the package identity
   string (e.g. "1ED5AEA5.4160926B82DB"), never meant for display. (An
   earlier version of this used the `Get-AppxPackageManifest` cmdlet
   instead, which silently never worked under this agent's LocalSystem
   service context — every resolution failed invisibly, so no package ever
   got a resolved name. Reading the manifest file straight off disk sidesteps
   that entirely.) Falls back to the identity string on any resolution
   failure, so a device is never left with a missing app just because one
   package's manifest couldn't be read. The reported `identifier` is
   unaffected either way — it's always the lowercased package identity name,
   matching Applivery UEM's own AppInventoryResults-sourced identifier for
   the same package. `installLocation` (the on-disk install path) is also
   reported when known — always for AppX packages, and for classic Win32
   installs only when the installer wrote one to the registry's `Uninstall`
   key — purely informational, shown in SOAR's App detail modal.

A serial number that's empty or a known placeholder (`UNKNOWN`,
`To Be Filled By O.E.M.`, `Default string`, etc.) is treated as unusable —
the agent skips that cycle's report instead of risking two different
degenerate-serial machines silently overwriting each other's data on the
backend.

---

## Custom Device Checks

Beyond the fixed telemetry above, an admin can define arbitrary checks in
**Settings → Custom Device Checks** that this agent runs locally every
cycle. Each check has a `checkerType` and its own `params`:

| Checker type | What it does | Windows implementation |
| :--- | :--- | :--- |
| `processRunning` | Is a named process currently running? | WMI `Win32_Process` exact-name match |
| `serviceStatus` | Is a named Windows service running? | Service Control Manager query (`golang.org/x/sys/windows/svc/mgr`) |
| `registryOrFileValue` | Read a registry value | `HKLM`/`HKCU` only; string value, falling back to integer formatted as decimal |
| `appInstalled` | Is an app installed, and what version? | Reuses the same inventory scan as `ReportApps` |
| `command` | Run an arbitrary PowerShell command and report its output | `powershell.exe -NoProfile -NonInteractive -Command "<command>"`, 30s timeout, output capped at 4KB |

A check failing to *run* (service not found, registry key missing, command
timeout) is reported as an **error**, which the backend's compliance
evaluator treats the same as "no data yet." A legitimately negative result —
a process that simply isn't running — is a normal value, not an error. The
`command` checker type runs exactly what's typed into the dashboard with the
agent's own (`LocalSystem`) privileges and no sandboxing — Settings surfaces
an explicit warning about this; use it deliberately.

---

## Webhook Endpoints & Payload Structure

* **Reports:** `POST <BaseURL>/api/device-data/report` and, when
  `ReportApps=1`, `POST <BaseURL>/api/device-data/report-apps`.
* **Custom check definitions poll:** `GET <BaseURL>/api/device-data/custom-checks?platform=windows`.
* **Headers on every request:**
  * `Content-Type: application/json` (report calls only)
  * `X-Workspace-Slug: <WorkspaceSlug>`
  * `X-Device-Report-Secret: <ReportSecret>` — omitted once this device has
    completed mTLS registration (see [mTLS Agent
    Authentication](#mtls-agent-authentication) above); the client
    certificate authenticates the request instead.

### Device report payload

```json
{
  "platform": "windows",
  "serialNumber": "PF3ABCDE",
  "attributes": {
    "OsBuild": "22631.3527",
    "OsEdition": "Pro",
    "BitLockerStatus": true,
    "FirewallEnabled": true
  },
  "customCheckResults": {
    "edr-running": { "value": true },
    "disk-encryption-key": { "error": "registry key not found: ..." }
  }
}
```

`customCheckResults` is omitted entirely (not sent as an empty object) when
no checks are configured, so the backend keeps whatever it already had
rather than wiping it.

### App inventory payload (`ReportApps=1`)

```json
{
  "platform": "windows",
  "serialNumber": "PF3ABCDE",
  "apps": [
    { "identifier": "Mozilla.Firefox", "name": "Mozilla Firefox", "version": "128.0", "origin": "winget" },
    {
      "identifier": "1ed5aea5.4160926b82db",
      "name": "Angry Birds 2",
      "version": "3.17.10.0",
      "origin": "store",
      "installLocation": "C:\\Program Files\\WindowsApps\\1ED5AEA5.4160926B82DB_3.17.10.0_x64__p2gbknwb5d8r2"
    }
  ]
}
```

`origin` is optional — `"winget"` for apps detected via `winget list`, `"msi"` for the registry Uninstall-key fallback (only used when winget itself isn't invokable), `"store"` for AppX/UWP packages (PowerShell `Get-AppxPackage`-sourced). Omitted entirely on older agent builds that predate this field; the backend treats a missing `origin` the same as it always has.

`installLocation` is optional and purely informational (shown in SOAR's App detail modal, never used for identifier/matching logic) — always present for AppX/Store packages, present for classic Win32 installs only when the installer wrote one to the registry's `Uninstall` key, and never present for winget-sourced entries.

---

## Building From Source

```powershell
git clone https://github.com/raulcnadal/applivery-soar-agent-windows.git
cd applivery-soar-agent-windows

go build -ldflags="-H windowsgui -s -w" -o Applivery-SOAR-Agent.exe .
go build -ldflags="-H windowsgui -s -w" -o Applivery-SOAR-Watchdog.exe ./watchdog
go build -ldflags="-H windowsgui -s -w" -o Applivery-SOAR-Tray.exe ./tray

dotnet tool install --global wix --version "4.*"
wix build -arch x64 -out Applivery-SOAR-Agent-amd64.msi agent.wxs
```

All three exes must be present in the repo root before `wix build` runs —
`agent.wxs` references all three by filename. Cross-compile for ARM64 by
setting `GOOS=windows GOARCH=arm64` before each `go build` step and passing
`-arch arm64` to `wix build`. `.github/workflows/build.yml`
does exactly this for both architectures on every push, and — on pushes to
`main` — also publishes both MSIs to the SOAR backend's zero-config download
endpoint (gated by the `SOAR_AGENT_BUILD_SECRET` repository secret, each
tagged with its own `X-Agent-Arch` header so the backend keeps them as two
distinct downloads) and republishes a rolling `latest` GitHub Release with
both MSIs attached.

---

## Deployment via UEM

1. **Upload** `Applivery-SOAR-Agent-amd64.msi` (or the ARM64 build, for
   ARM64 devices) as a Windows line-of-business app, installed
   Device/System-wide (per-machine).
2. **Push Managed Configuration** — deploy the `.reg` file from Settings →
   Applivery SOAR Agent via your UEM's registry/custom-configuration
   mechanism, targeting `HKLM\SOFTWARE\Policies\Applivery\SOAR`.
3. **Assign** the app and the configuration profile to your target device
   groups. The installer registers and starts the `AppliverySOARAgent` and
   `AppliverySOARWatchdog` services, and registers the `Applivery SOAR Tray`
   Scheduled Task, all automatically — no reboot required. The tray icon
   itself appears the next time each user logs on (Scheduled Tasks with an
   `ONLOGON` trigger don't retroactively fire for sessions already open at
   install time).

---

## Troubleshooting

* **Service status:** open `services.msc` and confirm **Applivery SOAR
  Agent** and **Applivery SOAR Watchdog** are both `Running` with
  **Automatic** startup. Stopping either one by hand should show it back as
  `Running` within about 30 seconds (see *Tamper resistance* above) — if it
  doesn't, check that service's own log (`watchdog.log`) for what it's
  seeing when it tries.
* **Tray icon missing:** confirm the Scheduled Task exists and is enabled —
  `schtasks /Query /TN "Applivery SOAR Tray"` — and that
  `Applivery-SOAR-Tray.exe` is actually running for the logged-in user
  (`Get-Process Applivery-SOAR-Tray`). The task itself only fires at logon,
  but **Applivery SOAR Watchdog** also polls for the tray process every 30
  seconds and silently re-runs the Scheduled Task if it's found missing (see
  `internal/svcwatch/tray.go`) — a tray process killed, crashed, or closed
  mid-session should reappear within that window on its own; it should only
  actually stay gone if nobody's logged on to the console, or the Watchdog
  service itself isn't running.
* **Logs:** every component writes its own file under
  `%ProgramData%\Applivery\SOAR\` — `agent.log`, `watchdog.log`, and
  `tray.log` — same rotate-at-10MB behavior for all three. The agent also
  writes `status.json` there after every report cycle (what the tray icon
  reads); if the tray's compliance section shows "unavailable", check
  `status.json`'s own `compliance.reason` field or `agent.log` for why the
  `GET /api/device-data/agent-status` call failed (commonly: no Automation
  Credential configured yet for this workspace under Settings).

  `agent.log` specifically is worth pulling first for most issues, whether
  the service is running normally or you're testing interactively —
  including over a remote session or via a script pushed through your UEM
  (`Get-Content 'C:\ProgramData\Applivery\SOAR\agent.log' -Tail 50`). Every
  cycle logs the
  resolved Managed Configuration (`Config loaded: BaseURL=... WorkspaceSlug=...
  ReportSecret=(set, N chars)...` — the secret itself is never logged) so you
  can immediately tell whether the registry key was actually read, and the
  HTTP result of each report attempt (`sent successfully -> HTTP Status 200`,
  or the exact non-2xx status / network error otherwise).
* **Manual/interactive run:** this build is linked `-H windowsgui` (so no
  console window flashes up when SCM starts it as a service), which means
  running the exe directly from PowerShell/cmd does **not** print anything
  to that terminal — it's a GUI-subsystem process, so it detaches from the
  launching console entirely and returns you to the prompt immediately, with
  the agent continuing to run invisibly in the background. That's expected,
  not a hang or a crash. `agent.log` is still the one place to watch —
  tail it in a second window while it runs:

  ```powershell
  Stop-Service AppliverySOARAgent
  Start-Process .\Applivery-SOAR-Agent.exe
  Get-Content 'C:\ProgramData\Applivery\SOAR\agent.log' -Wait -Tail 20
  ```

  Find and stop the detached process when you're done (`Get-Process
  Applivery-SOAR-Agent | Stop-Process`), then `Start-Service
  AppliverySOARAgent` to put the real service back.

* **"No WorkspaceSlug/ReportSecret" in the logs:** the registry key hasn't
  been populated yet — see *Configuration Reference* above. Confirm what's
  actually there with:

  ```powershell
  Get-ItemProperty 'HKLM:\SOFTWARE\Policies\Applivery\SOAR'
  ```

* **mTLS registration never seems to happen despite a `BootstrapToken` being
  set:** first confirm `IsConfigured()` is actually letting the report loop
  run at all — check the "Config loaded: ..." line in `agent.log`; if
  `WorkspaceSlug` is set and EITHER `ReportSecret` or `BootstrapToken` shows
  as "(set, N chars)", the loop runs. Then check for lines starting `mTLS:`
  — the most common causes are: the workspace not yet having a Certificate
  Authority configured, no Global Bootstrap Token generated yet (Settings →
  mTLS Agent Authentication), the token value not matching what's in the
  registry, or this device's serial number not yet visible in Applivery's
  own device list (the backend rejects a serial number it doesn't recognize
  as enrolled — give Applivery UEM's sync a few minutes after enrolling a
  new device). Registration fails closed with a clear log line in every
  case, same tolerance as every other best-effort step in this agent — it
  just keeps using whatever auth it already has and retries next cycle.
* **Config was just pushed but the log still shows the old values:** as of
  this build, Managed Configuration is re-read from the registry on every
  report cycle (default hourly) — no service restart needed, it'll pick it
  up on the next tick. Older builds cached the config once at service start;
  if you're troubleshooting a device that's been running since before this
  change shipped, `Restart-Service AppliverySOARAgent` to force an immediate
  reload rather than waiting out the interval.
* **Device shows "No security attestation reported" in SOAR despite the
  agent's own logs showing a successful POST:** the backend matches reports
  to a device by exact, case-sensitive serial number. Compare the
  `serialNumber` this agent is sending (visible in the log line for each
  report, or via `wmic bios get serialnumber` / `Get-CimInstance
  Win32_BIOS | select SerialNumber`) against the serial Applivery shows for
  that device in its own inventory — any difference in case, spacing, or
  formatting will silently prevent the match.
