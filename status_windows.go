//go:build windows
// +build windows

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/raulcnadal/applivery-soar-agent-windows/internal/agentstatus"
)

// writeStatusCache is best-effort by design — a device whose tray icon
// can't show fresh info because of a transient disk error is a much smaller
// problem than the reporting cycle itself failing, so failures here are
// logged and swallowed rather than propagated. See agentstatus.StatusCache's
// doc comment for why this file exists and who reads it.
func writeStatusCache(c agentstatus.StatusCache) {
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		log.Printf("Could not marshal status cache: %v", err)
		return
	}
	dir := agentstatus.Dir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("Could not create status cache directory %s: %v", dir, err)
		return
	}
	if err := os.WriteFile(agentstatus.CachePath(), data, 0644); err != nil {
		log.Printf("Could not write status cache to %s: %v", agentstatus.CachePath(), err)
	}
}

// fetchAgentStatus calls GET /api/device-data/agent-status — same auth
// headers as the report webhooks (sendWebhook, telemetry_windows.go). A
// non-2xx response or network error just returns an error; the caller
// (gatherAndReport) tolerates this by leaving the compliance section of the
// status cache as "unavailable" rather than failing the whole report cycle.
func fetchAgentStatus(baseURL *url.URL, config Config, serialNumber, platform string) (*agentstatus.AgentStatusResponse, error) {
	statusURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/agent-status"})
	q := statusURL.Query()
	q.Set("serialNumber", serialNumber)
	q.Set("platform", platform)
	statusURL.RawQuery = q.Encode()

	client := mtlsHTTPClient(15 * time.Second)
	req, err := http.NewRequest("GET", statusURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
	applyLegacyAuthIfNeeded(req, config)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("agent-status endpoint returned HTTP %d: %s", resp.StatusCode, responseBodySnippet(resp))
	}

	var result agentstatus.AgentStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// fetchAgentStatusWithRetry wraps fetchAgentStatus with a couple of quick,
// short-backoff retries within the same report cycle, rather than letting a
// single failure sit in the tray's status cache as "Unavailable" until the
// next full ticker interval — which, per clampInterval above, defaults to
// 1 hour and is admin-configurable arbitrarily higher. That gap is the
// actual explanation for a real-device report ("agent-status keeps showing
// Unavailable after an upgrade until I manually restart the service or
// reboot, which always works"): both of those actions short-circuit straight
// to runAgentLoop's initial gatherAndReport() call, i.e. an immediate retry
// — nothing about the service itself is stuck or failing to restart cleanly,
// it's just correctly waiting out its normal interval before trying again,
// which reads as "broken" when that interval is long and the one attempt
// that happened to run (e.g. right as the upgrade's brief network/backend
// hiccup was still settling) failed. Retrying a few times a short distance
// apart, still within this one background cycle, absorbs exactly that kind
// of transient failure without needing to know or fix its root cause, and
// without the admin ever noticing. If all attempts fail, the outcome is
// identical to before: Available=false with the same reason string.
func fetchAgentStatusWithRetry(baseURL *url.URL, config Config, serialNumber, platform string) (*agentstatus.AgentStatusResponse, error) {
	delays := []time.Duration{5 * time.Second, 15 * time.Second}
	status, err := fetchAgentStatus(baseURL, config, serialNumber, platform)
	if err == nil {
		return status, nil
	}
	for i, d := range delays {
		log.Printf("agent-status fetch failed (attempt %d/%d): %v — retrying in %s.", i+1, len(delays)+1, err, d)
		time.Sleep(d)
		status, err = fetchAgentStatus(baseURL, config, serialNumber, platform)
		if err == nil {
			return status, nil
		}
	}
	return nil, err
}

// readCurrentStatusCache reads back whatever this same process (or a prior
// run of it) last wrote to status.json via writeStatusCache — used by
// forceEvaluateCompliance to patch just the Compliance section in place
// rather than reconstructing the whole cache from scratch.
func readCurrentStatusCache() (*agentstatus.StatusCache, error) {
	data, err := os.ReadFile(agentstatus.CachePath())
	if err != nil {
		return nil, err
	}
	var cache agentstatus.StatusCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

// forceEvaluateCompliance is the service side of the tray's "Force evaluate
// compliance" button (checkTriggers, telemetry_windows.go): POSTs
// /api/device-data/evaluate-now and, on success, re-fetches this device's
// compliance status and patches it into the on-disk cache so the tray card
// shows the result the next time it's opened, without waiting for the next
// full report cycle. Best-effort throughout, same tolerance as
// updateStatusCache's own compliance fetch — a failed forced evaluation (no
// Automation Credential configured yet, the 60s cooldown in
// compliance.service.ts, a transient network error) is logged only; the
// device's next scheduled evaluation pass on the backend will still pick it
// up regardless of whether this particular forced attempt succeeded.
func forceEvaluateCompliance() {
	config := LoadConfig()
	if !config.IsConfigured() {
		log.Println("Force evaluate requested but this agent isn't configured yet (no WorkspaceSlug/ReportSecret) — ignoring.")
		return
	}
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		log.Printf("Force evaluate: invalid BaseURL in configuration: %v", err)
		return
	}

	evalURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/evaluate-now"})
	client := mtlsHTTPClient(30 * time.Second)
	req, err := http.NewRequest("POST", evalURL.String(), nil)
	if err != nil {
		log.Printf("Force evaluate: could not build request: %v", err)
		return
	}
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
	applyLegacyAuthIfNeeded(req, config)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Force evaluate: request failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("Force evaluate: backend returned HTTP %d: %s", resp.StatusCode, responseBodySnippet(resp))
		return
	}
	log.Println("Force evaluate: backend accepted the request — refreshing this device's compliance status.")

	serialNumber := GetSerialNumber()
	if !isUsableSerial(serialNumber) {
		return
	}
	status, err := fetchAgentStatus(baseURL, config, serialNumber, "windows")
	if err != nil {
		log.Printf("Force evaluate: could not re-fetch compliance status afterward: %v", err)
		return
	}

	cache, err := readCurrentStatusCache()
	if err != nil {
		// No cache on disk yet (agent hasn't completed a full cycle) —
		// nothing to patch; the next full report cycle will populate it.
		return
	}
	cache.Compliance = status.Compliance
	cache.DeviceMatched = status.Device.Matched
	if status.Device.DisplayName != nil {
		cache.DeviceName = *status.Device.DisplayName
	}
	writeStatusCache(*cache)
}
