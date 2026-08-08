//go:build windows
// +build windows

package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"time"
)

type DeviceData struct {
	Platform     string                 `json:"platform"`
	SerialNumber string                 `json:"serialNumber"`
	Attributes   map[string]interface{} `json:"attributes"`
}

// runAgentLoop runs continuously on a timer until Windows stops it
func runAgentLoop(config AgentConfig, stopChan <-chan struct{}) {
	log.Println("Agent loop started. Reporting data...")

	// We set a hardcoded 1-hour interval here. 
	// (Later, you can add IntervalSec to your registry config to control this from the UEM!)
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// Execute once immediately upon service startup
	gatherAndReport(config)

	for {
		select {
		case <-ticker.C:
			// The 1-hour timer went off
			gatherAndReport(config)
		case <-stopChan:
			// The Windows Service Manager sent a stop signal
			log.Println("Agent loop received stop signal. Shutting down gracefully.")
			return
		}
	}
}

// gatherAndReport contains all the WMI, Registry, and HTTP POST logic you already wrote
func gatherAndReport(config AgentConfig) {
	log.Println("Gathering telemetry...")

	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		log.Printf("Invalid BaseURL in configuration: %v", err)
		return
	}

	serialNumber := GetSerialNumber()
	client := &http.Client{Timeout: 15 * time.Second}

	// 1. Report Security & OS Attributes
	attributes := make(map[string]interface{})
	if config.ReportBitLocker {
		attributes["BitLockerStatus"] = GetBitLockerStatus()
	}
	if config.ReportFirewall {
		attributes["FirewallEnabled"] = GetFirewallStatus()
	}
	attributes["OsBuild"] = GetOSBuild()

	securityPayload := DeviceData{
		Platform:     "windows",
		SerialNumber: serialNumber,
		Attributes:   attributes,
	}

	sendReport(client, baseURL.JoinPath("/api/device-data/report").String(), config, securityPayload)

	// 2. Report Installed Software Inventory (if enabled)
	if config.ReportApps {
		appsPayload := AppsPayload{
			Platform:     "windows",
			SerialNumber: serialNumber,
			Apps:         GetInstalledApps(),
		}

		sendReport(client, baseURL.JoinPath("/api/device-data/report-apps").String(), config, appsPayload)
	}
}

func sendReport(client *http.Client, targetURL string, config AgentConfig, payload interface{}) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling JSON payload for %s: %v", targetURL, err)
		return
	}

	maxRetries := 3
	for i := 1; i <= maxRetries; i++ {
		req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("Fatal error creating HTTP request for %s: %v", targetURL, err)
			return 
		}

		// Inject headers dynamically from the UEM configuration
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
		req.Header.Set("X-Device-Report-Secret", config.ReportSecret) // No longer hardcoded!

		resp, err := client.Do(req)
		
		// Handle successful network transmission
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Printf("Report successfully sent to %s -> HTTP Status %d", targetURL, resp.StatusCode)
				return // Success, exit the retry loop
			}
			log.Printf("Attempt %d: Received non-success status %d from %s", i, resp.StatusCode, targetURL)
		} else {
			log.Printf("Attempt %d: Network error sending report to %s: %v", i, targetURL, err)
		}

		// If it failed and we haven't reached max retries, sleep and try again
		if i < maxRetries {
			sleepDuration := time.Duration(i*5) * time.Second // Simple backoff: wait 5s, then 10s
			log.Printf("Retrying in %v...", sleepDuration)
			time.Sleep(sleepDuration)
		}
	}
	
	log.Printf("Failed to send report to %s after %d attempts. Will try again next cycle.", targetURL, maxRetries)
}