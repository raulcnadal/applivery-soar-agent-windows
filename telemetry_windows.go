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
	"sync/atomic"
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

// remoteIntervalSecAtomic holds the latest Phase 4 SOAR-pushed report
// interval override (0 = none), set by gatherAndReport after each
// syncEventWatches call (eventwatch_windows.go) and read by
// maybeResetTicker below. Package-level + atomic rather than a return value
// threaded through every gatherAndReport call site: gatherAndReport is
// called from three places (runAgentLoop's initial call, its own ticker
// case, and checkTriggers' force-report path), and all three need the same
// "did the effective interval change, if so reset the ticker" follow-up —
// simplest to make that follow-up its own step (maybeResetTicker) that any
// caller can run after gatherAndReport, rather than threading a return
// value through checkTriggers too.
var remoteIntervalSecAtomic int32

// clampInterval mirrors the floor this file has always applied: below 30s
// is almost certainly a misconfiguration (or an admin-cleared value reading
// back as 0), so fall back to the original 1h default rather than hammering
// the backend.
func clampInterval(sec int) time.Duration {
	interval := time.Duration(sec) * time.Second
	if interval < 30*time.Second {
		interval = 3600 * time.Second
	}
	return interval
}

// maybeResetTicker recomputes the effective report interval — this
// device's local Managed Configuration IntervalSec, unless a Phase 4
// SOAR-pushed remoteIntervalSecAtomic override is present, in which case
// that wins — and calls ticker.Reset only when it actually changed. This
// closes two gaps at once: a local registry IntervalSec edit used to need
// an agent restart to take effect (this file's own prior doc comment called
// that "an acceptable trade-off"); now it doesn't, and neither does a
// SOAR-pushed override changing mid-session. Called after every
// gatherAndReport()/checkTriggers() in runAgentLoop's select loop below,
// not just on the ticker's own tick, so a force-report or trigger check
// also gets a chance to notice a change promptly.
func maybeResetTicker(ticker *time.Ticker, current time.Duration) time.Duration {
	sec := LoadConfig().IntervalSec
	if remote := atomic.LoadInt32(&remoteIntervalSecAtomic); remote > 0 {
		sec = int(remote)
	}
	next := clampInterval(sec)
	if next != current {
		log.Printf("Report interval changed: %s -> %s — resetting ticker.", current, next)
		ticker.Reset(next)
	}
	return next
}

// runAgentLoop no longer takes a Config: it used to be loaded once at
// service start and cached for the process's entire lifetime, which meant a
// Managed Configuration push (registry write) landing after the service was
// already running was silently ignored until someone manually restarted it.
// gatherAndReport now reloads the registry key fresh on every tick — same
// cadence Custom Device Checks already used — so a Policy/script deployed
// after install takes effect on the very next cycle with no restart needed.
// The report ticker itself now also hot-reloads via maybeResetTicker above
// (Phase 4) — IntervalSec changes, local or SOAR-pushed, take effect on the
// next loop iteration instead of needing a restart.
func runAgentLoop(stopChan <-chan struct{}) {
	log.Println("Agent loop started. Reporting data...")

	interval := clampInterval(LoadConfig().IntervalSec)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	triggerTicker := time.NewTicker(triggerPollInterval)
	defer triggerTicker.Stop()

	gatherAndReport()
	interval = maybeResetTicker(ticker, interval)

	for {
		select {
		case <-ticker.C:
			gatherAndReport()
			interval = maybeResetTicker(ticker, interval)
		case <-triggerTicker.C:
			checkTriggers()
			interval = maybeResetTicker(ticker, interval)
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

	// Event-driven change detection ("fast lane") — see eventwatch_windows.go's
	// top-of-file doc comment. Diffs the latest config against whichever
	// registry/ETW watchers are currently running and starts/stops/restarts
	// to match; those watchers then run independently of this ticker until
	// the next sync. Best-effort like everything else in this function — a
	// poll failure here just means this cycle's watcher state doesn't
	// change, not that the report itself fails. The returned remoteIntervalSec
	// (Phase 4, 0 = no override) is stashed for maybeResetTicker to pick up
	// right after this function returns (runAgentLoop's select loop).
	remoteIntervalSec := syncEventWatches(baseURL, config)
	atomic.StoreInt32(&remoteIntervalSecAtomic, int32(remoteIntervalSec))

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