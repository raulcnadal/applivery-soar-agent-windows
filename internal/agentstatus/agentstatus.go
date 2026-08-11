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
