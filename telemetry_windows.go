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

func runAgentLoop(config Config, stopChan <-chan struct{}) {
	log.Println("Agent loop started. Reporting data...")

	interval := time.Duration(config.IntervalSec) * time.Second
	if interval < 30*time.Second {
		interval = 3600 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	gatherAndReport(config)

	for {
		select {
		case <-ticker.C:
			gatherAndReport(config)
		case <-stopChan:
			log.Println("Agent loop received stop signal. Shutting down gracefully.")
			return
		}
	}
}

func gatherAndReport(config Config) {
	log.Println("Gathering telemetry...")

	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		log.Printf("Invalid BaseURL in configuration: %v", err)
		return
	}
	targetURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/report"}).String()

	serialNumber := GetSerialNumber()
	attributes := make(map[string]interface{})

	attributes["OsBuild"] = GetOSBuild()

	if config.ReportBitLocker {
		attributes["BitLockerStatus"] = GetBitLockerStatus()
	}
	if config.ReportFirewall {
		attributes["FirewallEnabled"] = GetFirewallStatus()
	}

	payload := DeviceData{
		Platform:     "windows",
		SerialNumber: serialNumber,
		Attributes:   attributes,
	}

	sendWebhook(targetURL, config, payload)

	if config.ReportApps {
		apps := GetInstalledApps()
		appsPayload := AppsPayload{
			Platform:     "windows",
			SerialNumber: serialNumber,
			Apps:         apps,
		}
		// If you have a separate apps endpoint, post appsPayload here similarly
		_ = appsPayload
	}
}

func sendWebhook(targetURL string, config Config, payload DeviceData) {
	client := &http.Client{Timeout: 15 * time.Second}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling JSON payload: %v", err)
		return
	}

	maxRetries := 3
	for i := 1; i <= maxRetries; i++ {
		req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("Fatal error creating HTTP request: %v", err)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
		req.Header.Set("X-Device-Report-Secret", config.ReportSecret)

		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Printf("Report successfully sent -> HTTP Status %d", resp.StatusCode)
				return
			}
			log.Printf("Attempt %d: Received non-success status %d", i, resp.StatusCode)
		} else {
			log.Printf("Attempt %d: Network error: %v", i, err)
		}

		if i < maxRetries {
			time.Sleep(time.Duration(i) * 5 * time.Second)
		}
	}
}