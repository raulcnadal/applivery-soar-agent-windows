//go:build windows
// +build windows

package main

import (
	"fmt"
	"log"

	"golang.org/x/sys/windows/registry"
)

type Config struct {
	BaseURL         string
	WorkspaceSlug   string
	ReportSecret    string
	ReportBitLocker bool
	ReportFirewall  bool
	ReportApps      bool
	IntervalSec     int
	// BootstrapToken — mTLS agent authentication (see mtls_windows.go and
	// backend/docs/mtls-agent-auth-roadmap.md). The SAME value pushed to
	// every device in the fleet via this Managed Configuration (a "Global
	// Bootstrap Token", not per-device or one-time — the backend checks it
	// against a live Applivery UEM serial-number lookup instead). Consumed
	// by ensureMtlsIdentity/registerMtlsIdentity on this device's first
	// successful registration (POST /api/device-mtls/register); not
	// expected to stay meaningful after that (the device renews on its own
	// certificate from then on), but unlike the old per-device one-time
	// design, leaving it in place is harmless — a device that already has
	// an active certificate is never silently re-registered.
	BootstrapToken string
}

// IsConfigured reports whether enough Managed Configuration was found to
// safely report anything. WorkspaceSlug has no default — unlike an earlier
// build of this agent, which shipped one real workspace's production secret
// hardcoded as the fallback here (baked into every compiled binary and
// readable in plaintext with `strings.exe`). Never hardcode a real secret as
// a compiled-in default again — it belongs exclusively in the Managed
// Configuration registry key, pushed per-fleet by whatever UEM deploys this
// agent.
//
// Either ReportSecret OR BootstrapToken alone is enough to proceed — an
// mTLS-only deployment (BootstrapToken set, ReportSecret intentionally
// blank) is a fully supported configuration, not a partial one: this used
// to hard-require ReportSecret unconditionally, which silently blocked
// ensureMtlsIdentity from ever running (gatherAndReport bails out before
// reaching it) on a device configured for bootstrap-token-only enrollment —
// a confirmed bug, not a deliberate gate.
func (c Config) IsConfigured() bool {
	return c.WorkspaceSlug != "" && (c.ReportSecret != "" || c.BootstrapToken != "")
}

func LoadConfig() Config {
	config := Config{
		BaseURL:         "https://soar.mi-labs.es",
		WorkspaceSlug:   "",
		ReportSecret:    "",
		ReportBitLocker: true,
		ReportFirewall:  true,
		ReportApps:      false,
		IntervalSec:     3600,
	}

	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Policies\Applivery\SOAR`, registry.QUERY_VALUE)
	if err != nil {
		log.Println("No Managed Configuration found in Registry — WorkspaceSlug plus either ReportSecret or BootstrapToken must be set at HKLM\\SOFTWARE\\Policies\\Applivery\\SOAR before this agent can report anything.")
		return config
	}
	defer k.Close()

	if val, _, err := k.GetStringValue("BaseURL"); err == nil && val != "" {
		config.BaseURL = val
	}
	if val, _, err := k.GetStringValue("WorkspaceSlug"); err == nil && val != "" {
		config.WorkspaceSlug = val
	}
	if val, _, err := k.GetStringValue("ReportSecret"); err == nil && val != "" {
		config.ReportSecret = val
	}
	if val, _, err := k.GetStringValue("BootstrapToken"); err == nil && val != "" {
		config.BootstrapToken = val
	}

	if val, _, err := k.GetIntegerValue("ReportBitLocker"); err == nil {
		config.ReportBitLocker = (val == 1)
	}
	if val, _, err := k.GetIntegerValue("ReportFirewall"); err == nil {
		config.ReportFirewall = (val == 1)
	}
	if val, _, err := k.GetIntegerValue("ReportApps"); err == nil {
		config.ReportApps = (val == 1)
	}
	if val, _, err := k.GetIntegerValue("IntervalSec"); err == nil && val > 0 {
		config.IntervalSec = int(val)
	}

	log.Printf(
		"Config loaded: BaseURL=%s WorkspaceSlug=%s ReportSecret=%s BootstrapToken=%s ReportBitLocker=%v ReportFirewall=%v ReportApps=%v IntervalSec=%d",
		config.BaseURL, maskEmpty(config.WorkspaceSlug), maskSecret(config.ReportSecret), maskSecret(config.BootstrapToken), config.ReportBitLocker, config.ReportFirewall, config.ReportApps, config.IntervalSec,
	)

	return config
}

// maskEmpty and maskSecret exist purely so LoadConfig's summary line is
// actually useful for troubleshooting "the script ran but nothing is being
// reported" — printed every cycle now that config is reloaded each tick
// (see gatherAndReport in telemetry_windows.go), never the raw secret.
func maskEmpty(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}
func maskSecret(s string) string {
	if s == "" {
		return "(not set)"
	}
	return fmt.Sprintf("(set, %d chars)", len(s))
}