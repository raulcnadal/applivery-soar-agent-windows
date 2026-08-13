//go:build windows
// +build windows

// Package agentstatus holds the data shapes shared between the main
// AppliverySOARAgent service (which writes them) and the tray helper (which
// reads them) — factored into its own package specifically so the two
// processes can never drift apart on field names/JSON shape the way two
// hand-copied struct definitions eventually would. AgentStatusResponse
// mirrors the backend's GET /api/device-data/agent-status response
// (backend/src/modules/devices/deviceData.service.ts); StatusCache is this
// repo's own on-disk cache format, written to
// %ProgramData%\Applivery\SOAR\status.json.
package agentstatus

import (
	"os"
	"path/filepath"
	"time"
)

// AgentStatusPolicy/AgentStatusViolation/AgentStatusCompliance/
// AgentStatusDevice/AgentStatusResponse mirror the backend's
// GET /api/device-data/agent-status JSON shape closely enough for
// encoding/json to populate them directly. This repo doesn't share a types
// package with the backend (different languages) — kept in sync by hand; a
// field the backend adds later is silently dropped here until this struct
// is updated to match, nothing panics.
type AgentStatusPolicy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Severity string `json:"severity"`
}

type AgentStatusViolation struct {
	PolicyID       string  `json:"policyId"`
	PolicyName     *string `json:"policyName"`
	Severity       *string `json:"severity"`
	LastDetectedAt *string `json:"lastDetectedAt"`
}

type AgentStatusCompliance struct {
	Available  bool                   `json:"available"`
	Reason     string                 `json:"reason,omitempty"`
	Compliant  bool                   `json:"compliant,omitempty"`
	RiskScore  *int                   `json:"riskScore,omitempty"`
	RiskTier   *string                `json:"riskTier,omitempty"`
	Policies   []AgentStatusPolicy    `json:"policies"`
	Violations []AgentStatusViolation `json:"violations,omitempty"`
}

type AgentStatusDevice struct {
	Matched     bool    `json:"matched"`
	ID          *string `json:"id"`
	DisplayName *string `json:"displayName"`
}

type AgentStatusResponse struct {
	Device     AgentStatusDevice     `json:"device"`
	Compliance AgentStatusCompliance `json:"compliance"`
}

// StatusCache is what the main agent writes to CachePath() after every
// report cycle that gets far enough to have something worth showing, and
// what the tray helper (a separate, unprivileged, per-user-session process
// with no registry/HTTP access of its own) reads to populate its
// right-click menu. Keeping the tray a pure reader of whatever the
// already-authenticated service last wrote avoids duplicating
// ReportSecret/registry/HTTP logic into a second process, and guarantees
// the tray always shows exactly what was actually last reported rather than
// a possibly-different live query of its own.
type StatusCache struct {
	UpdatedAt         string                `json:"updatedAt"`
	WorkspaceSlug     string                `json:"workspaceSlug"`
	BaseURL           string                `json:"baseUrl"`
	SerialNumber      string                `json:"serialNumber"`
	LastReportAt      string                `json:"lastReportAt"`
	LastReportOK      bool                  `json:"lastReportOk"`
	ReportedBitLocker bool                  `json:"reportedBitLocker"`
	BitLockerStatus   *bool                 `json:"bitLockerStatus,omitempty"`
	ReportedFirewall  bool                  `json:"reportedFirewall"`
	FirewallEnabled   *bool                 `json:"firewallEnabled,omitempty"`
	ReportedApps      bool                  `json:"reportedApps"`
	OsBuild           string                `json:"osBuild,omitempty"`
	DeviceMatched     bool                  `json:"deviceMatched"`
	DeviceName        string                `json:"deviceName,omitempty"`
	Compliance        AgentStatusCompliance `json:"compliance"`
}

// Dir returns %ProgramData%\Applivery\SOAR — the shared directory every
// component in this repo logs to, and where the status cache lives.
func Dir() string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "Applivery", "SOAR")
}

// CachePath returns the full path to status.json.
func CachePath() string {
	return filepath.Join(Dir(), "status.json")
}

// TriggerReportPath/TriggerEvaluatePath are marker files the tray helper
// writes to signal the main service: "the user clicked Force report" /
// "Force evaluate compliance". The tray process deliberately has no HTTP
// client or Managed Configuration secret of its own (see tray/main.go's
// doc comment) — it can't call the backend directly, so it drops an empty
// file here instead and lets the already-authenticated service act on it.
// The main agent loop polls for these on a short interval (see
// runAgentLoop/checkTriggers in telemetry_windows.go) and deletes each file
// the moment it's actioned, so a stale trigger can never fire twice and a
// tray process that writes one right as the service is mid-cycle just picks
// it up on the very next poll. File-based rather than a named pipe/socket to
// match this repo's existing status.json pattern — no new IPC primitive
// either process didn't already depend on.
func TriggerReportPath() string {
	return filepath.Join(Dir(), "trigger-report.flag")
}

func TriggerEvaluatePath() string {
	return filepath.Join(Dir(), "trigger-evaluate.flag")
}

// WriteTrigger creates (or refreshes) an empty marker file at path — the
// tray's side of the signal. Content is just an RFC3339 timestamp (useful
// when eyeballing %ProgramData%\Applivery\SOAR by hand); the service side
// only checks for the file's existence, never reads its contents.
func WriteTrigger(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0644)
}

// ConsumeTrigger reports whether a marker file exists at path and, if so,
// deletes it and returns true — the service side of the signal. A missing
// file (the common case, checked every couple of seconds) is not an error,
// just "nothing to do yet".
func ConsumeTrigger(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	_ = os.Remove(path)
	return true
}
