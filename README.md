```python
readme_content = """# Applivery SOAR Windows Agent

A lightweight, robust, native Go background service designed for Windows devices to automatically collect device telemetry and software inventory, and securely report them to the **Applivery SOAR** platform via Webhooks.

---

## Architecture & Design

The agent is compiled as a native 64-bit Windows executable (`Applivery-SOAR-Agent.exe`) and packaged into a modern **WiX v4 MSI installer**. 

* **Windows Service Wrapper:** Runs silently in the background as the `LocalSystem` account. This ensures high privileges are available to query secure system components (like BitLocker and Registry hives) and ensures continuous execution independently of user login sessions.
* **Managed Configuration (UEM Integration):** Instead of hardcoding endpoints or secrets, the agent reads its runtime configuration dynamically from the Windows Registry (`HKLM\\SOFTWARE\\Policies\\Applivery\\SOAR`). UEM platforms (such as Applivery, Intune, or Jamf) can push configuration profiles directly to this registry key.
* **Resilient Reporting Loop:** Wakes up on a configurable timer (`IntervalSec`, defaulting to 1 hour), gathers telemetry, and submits reports with built-in HTTP retry logic and exponential backoff.

---

## Configuration Reference (Managed Configuration)

Administrators can configure the agent by pushing values to the following Windows Registry path:
`HKLM\\SOFTWARE\\Policies\\Applivery\\SOAR`

| Registry Value Name | Type | Default Value | Description |
| :--- | :--- | :--- | :--- |
| `BaseURL` | String | `https://soar.mi-labs.es` | The base URL of your Applivery SOAR instance. |
| `WorkspaceSlug` | String | `friendly-emporium` | Your workspace identifier. |
| `ReportSecret` | String | `db4rLzdlJBo08SArnnH9pHZm` | The authentication secret token for webhook authorization. |
| `IntervalSec` | Integer | `3600` | Reporting interval in seconds (minimum recommended: 30s). |
| `ReportBitLocker` | Integer (1/0) | `1` (True) | Include BitLocker disk encryption telemetry. |
| `ReportFirewall` | Integer (1/0) | `1` (True) | Include Windows Firewall status telemetry. |
| `ReportApps` | Integer (1/0) | `0` (False) | Include installed application inventory scanning. |

---

## Telemetry & Data Collection

The agent natively queries Windows APIs, WMI, and the Registry without relying on external PowerShell scripts:

1. **Device Identity:** Retrieves the unique hardware serial number via WMI (`Win32_BIOS`).
2. **OS Build:** Queries the current OS build number (`SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion`).
3. **BitLocker Status:** Queries the `root\\CIMv2\\Security\\MicrosoftVolumeEncryption` WMI namespace to evaluate volume protection status.
4. **Firewall Status:** Checks the standard firewall profile configuration in the registry (`SYSTEM\\CurrentControlSet\\Services\\SharedAccess\\Parameters\\FirewallPolicy\\StandardProfile`).
5. **Installed Software Inventory:** Enumerates 64-bit, 32-bit (`WOW6432Node`), and User-level `Uninstall` registry keys to extract clean, deduplicated application names and versions.

---

## Webhook Endpoint & Payload Structure

The agent transmits data securely via HTTP POST requests to the Applivery SOAR ingestion endpoints:

* **Method:** `POST`
* **URL:** `<BaseURL>/api/device-data/report`
* **Headers:**
  * `Content-Type: application/json`
  * `X-Workspace-Slug: <WorkspaceSlug>`
  * `X-Device-Report-Secret: <ReportSecret>`

### 1. Security & Attributes Payload Example

```

```text
README.md generated successfully.

```json
{
  "platform": "windows",
  "serialNumber": "PF3ABCDE",
  "attributes": {
    "OsBuild": "22631.3527",
    "BitLockerStatus": true,
    "FirewallEnabled": true
  }
}

```

---

## Deployment & Installation Guide

### 1. Building the MSI Installer

Using WiX v4 and the provided `agent.wxs` configuration file, compile the 64-bit installer:

```powershell
wix build -arch x64 -out Applivery-SOAR-Agent-amd64.msi agent.wxs

```

### 2. UEM Deployment Steps

1. **Upload App:** Upload `Applivery-SOAR-Agent-amd64.msi` into your UEM platform as a Windows line-of-business (LoB) application. Set installation scope to **Device / System** (Per-Machine).
2. **Push Configuration:** Configure your UEM's Custom Configuration / Registry profile to seed `HKLM\\SOFTWARE\\Policies\\Applivery\\SOAR`.
3. **Deploy:** Assign the app and configuration profile to your target Windows device groups. The installer will automatically register the binary as a background Windows Service (`AppliverySOARAgent`), run under `LocalSystem`, and start reporting immediately.

---

## Troubleshooting & Verification

* **Verify Service Status:** Open `services.msc` on the target machine and confirm that **Applivery SOAR Agent** is running and set to **Automatic** startup.
* **Inspect Logs:** Check Windows Event Viewer or agent output logs for connectivity or configuration warnings.
* **Manual Testing:** Run the agent executable interactively in a command prompt for debug logging:
```powershell
.\\Applivery-SOAR-Agent.exe

```



"""

with open("README.md", "w", encoding="utf-8") as f:
f.write(readme_content)
print("README.md generated successfully.")

```
Your markdown file is ready
[file-tag: code-generated-file-0-1786268448283547097]

The generated **`README.md`** provides a comprehensive, production-grade guide covering every aspect of your Windows agent architecture, configuration schema, telemetry sources, webhook payload format, and UEM deployment workflow.

```