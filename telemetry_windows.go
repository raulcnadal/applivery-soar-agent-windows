//go:build windows
// +build windows

package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/raulcnadal/applivery-soar-agent-windows/internal/agentstatus"
)

// triggerPollInterval is how often runAgentLoop checks for a force-report/
// force-evaluate marker file left by the tray helper (agentstatus.
// WriteTrigger's doc comment has the full rationale). A plain os.Stat every
// couple of seconds is cheap enough to not need its own goroutine/channel —
// it just rides the same select alongside the normal report ticker.
const triggerPollInterval = 2 * time.Second

type DeviceData struct {
	Platform     string                 `json:"platform"`
	SerialNumber string                 `json:"serialNumber"`
	Attributes   map[string]interface{} `json:"attributes"`
	// Custom Device Checks results (customchecks_windows.go) — omitted
	// entirely (not an empty object) when there are no checks configured
	// for this workspace/platform, so the backend's reportDeviceData()
	// carries forward whatever it already had instead of wiping it.
	CustomCheckResults map[string]CustomCheckResult `json:"customCheckResults,omitempty"`
}

// isUsableSerial rejects the values GetSerialNumber() itself falls back to
// on failure ("UNKNOWN", empty) plus the handful of bogus BIOS defaults
// real-world OEMs still ship (seen on cheap/cloned/refurbished hardware).
// Reporting under any of these would silently collide with every other
// device on this same machine's fleet that also failed to read a real
// serial — the backend keys self-reported data by serial number, so two
// "UNKNOWN" devices overwrite each other's attributes/apps in place. Better
// to skip the report and let it show up in this agent's own logs than to
// attribute one device's data to another.
func isUsableSerial(serial string) bool {
	s := strings.ToUpper(strings.TrimSpace(serial))
	switch s {
	case "", "UNKNOWN", "TO BE FILLED BY O.E.M.", "DEFAULT STRING", "SYSTEM SERIAL NUMBER", "NOT SPECIFIED", "0", "NONE":
		return false
	default:
		return true
	}
}

// runAgentLoop no longer takes a Config: it used to be loaded once at
// service start and cached for the process's entire lifetime, which meant a
// Managed Configuration push (registry write) landing after the service was
// already running was silently ignored until someone manually restarted it.
// gatherAndReport now reloads the registry key fresh on every tick — same
// cadence Custom Device Checks already used — so a Policy/script deployed
// after install takes effect on the very next cycle with no restart needed.
// The initial load here only fixes the ticker's own interval for this
// process's lifetime; IntervalSec changes still need a restart to apply,
// which is an acceptable trade-off since a stuck/wrong interval is far less
// disruptive than "never picks up new config at all".
func runAgentLoop(stopChan <-chan struct{}) {
	log.Println("Agent loop started. Reporting data...")

	initial := LoadConfig()
	interval := time.Duration(initial.IntervalSec) * time.Second
	if interval < 30*time.Second {
		interval = 3600 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	triggerTicker := time.NewTicker(triggerPollInterval)
	defer triggerTicker.Stop()

	gatherAndReport()

	for {
		select {
		case <-ticker.C:
			gatherAndReport()
		case <-triggerTicker.C:
			checkTriggers()
		case <-stopChan:
			log.Println("Agent loop received stop signal. Shutting down gracefully.")
			return
		}
	}
}

// checkTriggers is the service side of the tray's "Force report"/"Force
// evaluate compliance" buttons — see agentstatus.WriteTrigger's doc comment
// for the full design. Each trigger file is consumed (deleted) the instant
// it's seen, so a click can never double-fire even if this tick and the
// tray's write race.
func checkTriggers() {
	if agentstatus.ConsumeTrigger(agentstatus.TriggerReportPath()) {
		log.Println("Force report triggered from the tray — running an immediate report cycle.")
		gatherAndReport()
	}
	if agentstatus.ConsumeTrigger(agentstatus.TriggerEvaluatePath()) {
		log.Println("Force evaluate triggered from the tray — requesting an immediate compliance evaluation.")
		forceEvaluateCompliance()
	}
}

func gatherAndReport() {
	config := LoadConfig()
	if !config.IsConfigured() {
		log.Println("No WorkspaceSlug/ReportSecret in Managed Configuration yet — skipping this cycle. Push HKLM\\SOFTWARE\\Policies\\Applivery\\SOAR to start reporting.")
		return
	}

	log.Println("Gathering telemetry...")

	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		log.Printf("Invalid BaseURL in configuration: %v", err)
		return
	}

	serialNumber := GetSerialNumber()
	if !isUsableSerial(serialNumber) {
		log.Printf("Serial number %q is empty or a known placeholder — skipping this report to avoid colliding with another device's data.", serialNumber)
		return
	}
	log.Printf("Reporting as serial number %q — must match Applivery's own inventory exactly (case-sensitive) for the backend to attach this data to the right device.", serialNumber)

	attributes := make(map[string]interface{})
	attributes["OsBuild"] = GetOSBuild()
	// Unconditional, like OsBuild above — this is basic inventory (which
	// edition is installed), not a security-posture toggle, so it doesn't
	// need its own Managed Configuration flag the way BitLocker/Firewall
	// reporting do. Omitted entirely (rather than sent as "") if the
	// registry read fails, so the backend/UI can tell "agent doesn't know"
	// apart from "no edition installed."
	if edition := GetOSEdition(); edition != "" {
		attributes["OsEdition"] = edition
	}
	if config.ReportBitLocker {
		attributes["BitLockerStatus"] = GetBitLockerStatus()
	}
	if config.ReportFirewall {
		attributes["FirewallEnabled"] = GetFirewallStatus()
	}

	// Custom Device Checks (Settings > Custom Device Checks) — polled fresh
	// every cycle, same as the fixed attributes above, so a check an admin
	// just created or edited takes effect on this device's very next report
	// without needing any Managed Configuration push.
	customCheckResults := runCustomChecks(fetchCustomChecks(baseURL, config))

	reportURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/report"}).String()
	payload := DeviceData{
		Platform:           "windows",
		SerialNumber:       serialNumber,
		Attributes:         attributes,
		CustomCheckResults: customCheckResults,
	}
	reportOK := sendWebhook(reportURL, config, payload)

	if config.ReportApps {
		apps := GetInstalledApps()
		appsPayload := AppsPayload{
			Platform:     "windows",
			SerialNumber: serialNumber,
			Apps:         apps,
		}
		appsURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/report-apps"}).String()
		sendWebhook(appsURL, config, appsPayload)
	}

	updateStatusCache(baseURL, config, serialNumber, attributes, reportOK)
}

// updateStatusCache is the tray icon's only data source (status_windows.go's
// StatusCache doc comment has the full rationale): a local summary of what
// this cycle just reported, plus a fresh pull of this device's Compliance
// Policy status/score from the backend. Best-effort throughout — a failed
// compliance fetch (no Automation Credential configured yet, a transient
// network error, whatever) still lets the "what we reported" half of the
// cache update, with Compliance.Available left false and a human-readable
// reason for the tray to display.
func updateStatusCache(baseURL *url.URL, config Config, serialNumber string, attributes map[string]interface{}, reportOK bool) {
	cache := agentstatus.StatusCache{
		WorkspaceSlug:     config.WorkspaceSlug,
		BaseURL:           config.BaseURL,
		SerialNumber:      serialNumber,
		LastReportAt:      time.Now().UTC().Format(time.RFC3339),
		LastReportOK:      reportOK,
		ReportedBitLocker: config.ReportBitLocker,
		ReportedFirewall:  config.ReportFirewall,
		ReportedApps:      config.ReportApps,
	}
	if osBuild, ok := attributes["OsBuild"].(string); ok {
		cache.OsBuild = osBuild
	}
	if v, ok := attributes["BitLockerStatus"].(bool); ok {
		cache.BitLockerStatus = &v
	}
	if v, ok := attributes["FirewallEnabled"].(bool); ok {
		cache.FirewallEnabled = &v
	}

	status, err := fetchAgentStatus(baseURL, config, serialNumber, "windows")
	if err != nil {
		log.Printf("Could not fetch compliance status for the tray icon: %v", err)
		cache.Compliance = agentstatus.AgentStatusCompliance{Available: false, Reason: "Could not reach the SOAR backend for compliance status."}
	} else {
		cache.Compliance = status.Compliance
		cache.DeviceMatched = status.Device.Matched
		if status.Device.DisplayName != nil {
			cache.DeviceName = *status.Device.DisplayName
		}
	}

	writeStatusCache(cache)
}

// sendWebhook is shared by both the attributes report and the (optional)
// app-inventory report — same endpoint family (POST /api/device-data/*),
// same header pair, same retry/backoff policy. Accepts any JSON-marshalable
// payload so both DeviceData and AppsPayload can reuse it. Returns whether
// the report ultimately succeeded (used by gatherAndReport to record
// LastReportOK in the tray's status cache) — every caller before the status
// cache feature ignored this return value entirely, so the behavior for
// existing callers is unchanged.
func sendWebhook(targetURL string, config Config, payload interface{}) bool {
	client := &http.Client{Timeout: 15 * time.Second}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling JSON payload: %v", err)
		return false
	}

	maxRetries := 3
	for i := 1; i <= maxRetries; i++ {
		req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("Fatal error creating HTTP request: %v", err)
			return false
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
		req.Header.Set("X-Device-Report-Secret", config.ReportSecret)

		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Printf("Report to %s sent successfully -> HTTP Status %d", targetURL, resp.StatusCode)
				return true
			}
			log.Printf("Attempt %d: %s returned non-success status %d", i, targetURL, resp.StatusCode)
		} else {
			log.Printf("Attempt %d: Network error POSTing to %s: %v", i, targetURL, err)
		}

		if i < maxRetries {
			time.Sleep(time.Duration(i) * 5 * time.Second)
		}
	}
	return false
}