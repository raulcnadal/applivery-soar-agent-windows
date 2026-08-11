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

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", statusURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
	req.Header.Set("X-Device-Report-Secret", config.ReportSecret)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("agent-status endpoint returned HTTP %d", resp.StatusCode)
	}

	var result agentstatus.AgentStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
