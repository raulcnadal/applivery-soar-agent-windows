//go:build windows
// +build windows

package main

import (
	"log"

	"golang.org/x/sys/windows/registry"
)

type AgentConfig struct {
	BaseURL         string
	WorkspaceSlug   string
	ReportSecret    string // <-- Added this field
	ReportBitLocker bool
	ReportFirewall  bool
	ReportApps      bool
}

func LoadConfig() AgentConfig {
	// 1. Safe defaults in case the UEM hasn't pushed the config yet
	config := AgentConfig{
		BaseURL:         "https://soar.mi-labs.es",
		WorkspaceSlug:   "friendly-emporium",
		ReportSecret:    "db4rLzdlJBo08SArnnH9pHZm", // <-- Added safe default
		ReportBitLocker: true,
		ReportFirewall:  true,
		ReportApps:      false,
	}

	// 2. Open the Registry Key where the UEM drops the configuration
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Policies\Applivery\SOAR`, registry.QUERY_VALUE)
	if err != nil {
		log.Println("No Managed Configuration found in Registry. Using default settings.")
		return config
	}
	defer k.Close()

	// 3. Overwrite defaults with any values found in the Registry
	if val, _, err := k.GetStringValue("BaseURL"); err == nil && val != "" {
		config.BaseURL = val
	}
	if val, _, err := k.GetStringValue("WorkspaceSlug"); err == nil && val != "" {
		config.WorkspaceSlug = val
	}
	// Fetch the dynamic secret from the UEM
	if val, _, err := k.GetStringValue("ReportSecret"); err == nil && val != "" {
		config.ReportSecret = val
	}
	
	// Boolean toggles (1 = true, 0 = false)
	if val, _, err := k.GetIntegerValue("ReportBitLocker"); err == nil {
		config.ReportBitLocker = val == 1
	}
	if val, _, err := k.GetIntegerValue("ReportFirewall"); err == nil {
		config.ReportFirewall = val == 1
	}
	if val, _, err := k.GetIntegerValue("ReportApps"); err == nil {
		config.ReportApps = val == 1
	}

	return config
}