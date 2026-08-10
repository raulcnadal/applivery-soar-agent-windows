# Applivery SOAR Agent — Windows

A lightweight, native Go background service for Windows devices. It collects
security posture, device telemetry, and (optionally) software inventory and
custom admin-defined checks, then reports them to an
[Applivery SOAR](https://github.com/raulcnadal/applivery-soar) instance,
where they become available as **Compliance Policy** conditions and
Overview/Devices telemetry.

The agent is compiled into a native 64-bit or ARM64 executable
(`Applivery-SOAR-Agent.exe`) and packaged as a **WiX v4 MSI installer**
(`Applivery-SOAR-Agent-amd64.msi` / `-arm64.msi`) that registers itself as
the `AppliverySOARAgent` Windows Service, running under `LocalSystem`.

---

## Getting the binary

You don't need to build this yourself, and you don't need a GitHub token —
the compiled MSI is downloadable straight from your SOAR instance:

**Settings → Device Data Webhook → Applivery SOAR Agent**, click **Download**
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
  1 hour — 3600s), gathers telemetry, and POSTs it with retry + backoff.
* **Custom Device Checks:** Once per cycle, before reporting, the agent polls
  the backend for whatever checks an admin has defined for Windows in
  **Settings → Custom Device Checks**, runs each one locally, and includes
  the results in the same report — no separate call, no local state kept
  between cycles. A check created or edited in the dashboard takes effect on
  this device's very next report.

---

## Configuration Reference (Managed Configuration)

All values live under `HKLM\SOFTWARE\Policies\Applivery\SOAR`. There is no
compiled-in default for `WorkspaceSlug` or `ReportSecret` — until both are
set, the agent logs a warning each cycle and reports nothing.

| Registry Value | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `BaseURL` | String | `https://soar.mi-labs.es` | Base URL of your Applivery SOAR instance. |
| `WorkspaceSlug` | String | *(none — required)* | Your workspace identifier. |
| `ReportSecret` | String | *(none — required)* | Device-report webhook secret (Settings → Device Data Webhook → Generate webhook secret). |
| `IntervalSec` | DWORD | `3600` | Reporting interval in seconds (values under 30 fall back to the default). |
| `ReportBitLocker` | DWORD (1/0) | `1` | Include BitLocker disk-encryption status. |
| `ReportFirewall` | DWORD (1/0) | `1` | Include Windows Firewall status. |
| `ReportApps` | DWORD (1/0) | `0` | Include the full installed-application inventory. |

Settings → Device Data Webhook generates a ready-to-import `.reg` file with
all of these pre-filled for your workspace — you shouldn't need to type any
of this by hand.

---

## Telemetry & Data Collection

Everything is read natively via Win32/WMI/Registry APIs — no PowerShell
scripts are shelled out to for the built-in telemetry (Custom Device Checks'
`command` checker type is the one deliberate exception; see below):

1. **Device identity** — hardware serial number via WMI `Win32_BIOS`.
2. **OS build** — `SOFTWARE\Microsoft\Windows NT\CurrentVersion`.
3. **BitLocker status** — `root\CIMv2\Security\MicrosoftVolumeEncryption`.
4. **Firewall status** — `SYSTEM\CurrentControlSet\Services\SharedAccess\Parameters\FirewallPolicy\StandardProfile`.
5. **Installed software** (when `ReportApps=1`) — 64-bit, 32-bit
   (`WOW6432Node`), and per-user `Uninstall` registry keys, deduplicated into
   clean name/version pairs.

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
  * `X-Device-Report-Secret: <ReportSecret>`

### Device report payload

```json
{
  "platform": "windows",
  "serialNumber": "PF3ABCDE",
  "attributes": {
    "OsBuild": "22631.3527",
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
    { "identifier": "Google Chrome", "name": "Google Chrome", "version": "125.0.6422.113" }
  ]
}
```

---

## Building From Source

```powershell
git clone https://github.com/raulcnadal/applivery-soar-agent-windows.git
cd applivery-soar-agent-windows

go build -ldflags="-H windowsgui -s -w" -o Applivery-SOAR-Agent.exe .

dotnet tool install --global wix --version "4.*"
wix build -arch x64 -out Applivery-SOAR-Agent-amd64.msi agent.wxs
```

Cross-compile for ARM64 by setting `GOOS=windows GOARCH=arm64` before the
`go build` step and passing `-arch arm64` to `wix build`. `.github/workflows/build.yml`
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
   Device Data Webhook via your UEM's registry/custom-configuration
   mechanism, targeting `HKLM\SOFTWARE\Policies\Applivery\SOAR`.
3. **Assign** the app and the configuration profile to your target device
   groups. The installer registers and starts the `AppliverySOARAgent`
   service automatically — no reboot required.

---

## Troubleshooting

* **Service status:** open `services.msc` and confirm **Applivery SOAR
  Agent** is `Running` with **Automatic** startup.
* **Logs:** the service logs via the standard Go `log` package to its
  process output — check Windows Event Viewer's Application log, or run the
  binary interactively (below) for logging directly to the console.
* **Manual/interactive run** (useful for debugging Managed Configuration —
  the agent detects it isn't running as a service and logs to the console
  instead, and `Ctrl+C` shuts it down cleanly):

  ```powershell
  .\Applivery-SOAR-Agent.exe
  ```

* **"No WorkspaceSlug/ReportSecret" in the logs:** the registry key hasn't
  been populated yet — see *Configuration Reference* above.
