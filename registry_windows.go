//go:build windows
// +build windows

package main

import (
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
}

// IsConfigured reports whether enough Managed Configuration was found to
// safely report anything. WorkspaceSlug/ReportSecret have no default —
// unlike an earlier build of this agent, which shipped one real workspace's
// production secret hardcoded as the fallback here (baked into every
// compiled binary and readable in plaintext with `strings.exe`). Never
// hardcode a real secret as a compiled-in default again — it belongs
// exclusively in the Managed Configuration registry key, pushed per-fleet
// by whatever UEM deploys this agent.
func (c Config) IsConfigured() bool {
	return c.WorkspaceSlug != "" && c.ReportSecret != ""
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
		log.Println("No Managed Configuration found in Registry — WorkspaceSlug/ReportSecret must be set at HKLM\\SOFTWARE\\Policies\\Applivery\\SOAR before this agent can report anything.")
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

	return config
}