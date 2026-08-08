package main

import (
	"encoding/json"
	"log"
	"os"
)

type Config struct {
	WebhookUrl    string `json:"webhook_url"`
	WorkspaceSlug string `json:"workspace_slug"`
	ReportSecret  string `json:"report_secret"`
	IntervalSec   int    `json:"interval_sec"`
}

func LoadConfig() Config {
	// 1. Define safe defaults
	cfg := Config{
		WebhookUrl:    "https://soar.mi-labs.es/api/device-data/report",
		WorkspaceSlug: "friendly-emporium",
		ReportSecret:  "db4rLzdlJBo08SArnnH9pHZm",
		IntervalSec:   3600, // 1 hour
	}

	// 2. Read UEM Managed Config (if it exists)
	configPath := "C:\\ProgramData\\Applivery\\config.json"
	file, err := os.Open(configPath)
	if err != nil {
		log.Printf("No managed config found at %s, using defaults.", configPath)
		return cfg
	}
	defer file.Close()

	// 3. Parse and overwrite defaults with UEM values
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		log.Printf("Failed to parse config: %v. Using defaults.", err)
	}

	return cfg
}