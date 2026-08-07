//go:build windows
// +build windows

package main

import (
	"log"

	"golang.org/x/sys/windows/registry"
)

// AgentConfig holds the dynamic settings provided by the UEM
type AgentConfig struct {
	ReportBitLocker bool
	ReportFirewall  bool
	WorkspaceSlug   string
}

// LoadConfig reads the Managed Configuration from the Windows Registry
func LoadConfig() AgentConfig {
	// Default configuration fallbacks if the UEM hasn't pushed keys yet
	config := AgentConfig{
		ReportBitLocker: true,
		ReportFirewall:  true,
		WorkspaceSlug:   "default-workspace",
	}

	// UEM managed configurations typically land under HKLM\SOFTWARE\Policies
	// We will define a custom subkey for your agent. 
	// Adjust "Applivery\SOAR" to match exactly how your UEM deploys the payload.
	registryPath := `SOFTWARE\Policies\Applivery\SOAR`

	key, err := registry.OpenKey(registry.LOCAL_MACHINE, registryPath, registry.QUERY_VALUE)
	if err != nil {
		log.Printf("Notice: Managed Configuration registry key not found at HKLM\\%s. Using defaults.", registryPath)
		return config
	}
	defer key.Close()

	// Read ReportBitLocker toggle (1 = true, 0 = false)
	if val, _, err := key.GetIntegerValue("ReportBitLocker"); err == nil {
		config.ReportBitLocker = val != 0
	}

	// Read ReportFirewall toggle (1 = true, 0 = false)
	if val, _, err := key.GetIntegerValue("ReportFirewall"); err == nil {
		config.ReportFirewall = val != 0
	}

	// Read Workspace Slug string
	if val, _, err := key.GetStringValue("WorkspaceSlug"); err == nil {
		config.WorkspaceSlug = val
	}

	log.Println("Successfully loaded Managed Configuration from Registry.")
	return config
}