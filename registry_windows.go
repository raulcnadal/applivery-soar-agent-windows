//go:build windows
// +build windows

package main

import (
	"log"
	"golang.org/x/sys/windows/registry"
)

type AgentConfig struct {
	ReportBitLocker bool
	ReportFirewall  bool
	ReportApps      bool
	WorkspaceSlug   string
	BaseURL         string
}

func LoadConfig() AgentConfig {
	config := AgentConfig{
		ReportBitLocker: true,
		ReportFirewall:  true,
		ReportApps:      true,
		WorkspaceSlug:   "default-workspace",
		BaseURL:         "https://soar.mi-labs.es",
	}

	registryPath := `SOFTWARE\Policies\Applivery\SOAR`

	key, err := registry.OpenKey(registry.LOCAL_MACHINE, registryPath, registry.QUERY_VALUE)
	if err != nil {
		log.Printf("Notice: Managed Configuration key not found at HKLM\\%s. Using defaults.", registryPath)
		return config
	}
	defer key.Close()

	if val, _, err := key.GetIntegerValue("ReportBitLocker"); err == nil {
		config.ReportBitLocker = val != 0
	}
	if val, _, err := key.GetIntegerValue("ReportFirewall"); err == nil {
		config.ReportFirewall = val != 0
	}
	if val, _, err := key.GetIntegerValue("ReportApps"); err == nil {
		config.ReportApps = val != 0
	}
	if val, _, err := key.GetStringValue("WorkspaceSlug"); err == nil {
		config.WorkspaceSlug = val
	}
	if val, _, err := key.GetStringValue("BaseURL"); err == nil && val != "" {
		config.BaseURL = val
	}

	log.Println("Successfully loaded Managed Configuration from Registry.")
	return config
}