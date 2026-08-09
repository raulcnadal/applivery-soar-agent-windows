//go:build !windows
// +build !windows

package main

import (
	"encoding/json"
	"log"
	"os"
)

type Config struct {
	BaseURL         string `json:"base_url"`
	WorkspaceSlug   string `json:"workspace_slug"`
	ReportSecret    string `json:"report_secret"`
	IntervalSec     int    `json:"interval_sec"`
	ReportBitLocker bool   `json:"report_bitlocker"`
	ReportFirewall  bool   `json:"report_firewall"`
	ReportApps      bool   `json:"report_apps"`
}

func LoadConfig() Config {
	cfg := Config{
		BaseURL:         "https://soar.mi-labs.es",
		WorkspaceSlug:   "friendly-emporium",
		ReportSecret:    "db4rLzdlJBo08SArnnH9pHZm",
		IntervalSec:     3600,
		ReportBitLocker: true,
		ReportFirewall:  true,
		ReportApps:      false,
	}

	configPath := "/Library/Preferences/es.mi-labs.soar.agent.json"
	file, err := os.Open(configPath)
	if err != nil {
		log.Printf("No managed config found at %s, using defaults.", configPath)
		return cfg
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		log.Printf("Failed to parse config: %v. Using defaults.", err)
	}

	return cfg
}