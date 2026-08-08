//go:build windows
// +build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func main() {
	fmt.Println("Starting Applivery SOAR Windows Agent...")

	// 1. Load configuration from Registry
	config := LoadConfig()

	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		log.Fatalf("Invalid BaseURL in configuration: %v", err)
	}

	serialNumber := GetSerialNumber()
	client := &http.Client{Timeout: 15 * time.Second}

	// 2. Report Security & OS Attributes
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

	sendReport(client, baseURL.JoinPath("/api/device-data/report").String(), config.WorkspaceSlug, securityPayload)

	// 3. Report Installed Software Inventory (if enabled)
	if config.ReportApps {
		appsPayload := AppsPayload{
			Platform:     "windows",
			SerialNumber: serialNumber,
			Apps:         GetInstalledApps(),
		}

		sendReport(client, baseURL.JoinPath("/api/device-data/report-apps").String(), config.WorkspaceSlug, appsPayload)
	}

	log.Println("Applivery SOAR Agent execution completed.")
}

func sendReport(client *http.Client, targetURL, workspaceSlug string, payload interface{}) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling JSON payload for %s: %v", targetURL, err)
		return
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Error creating HTTP request for %s: %v", targetURL, err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workspace-Slug", workspaceSlug)
	req.Header.Set("X-Device-Report-Secret", "db4rLzdlJBo08SArnnH9pHZm")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to send report to %s: %v", targetURL, err)
		return
	}
	defer resp.Body.Close()

	log.Printf("Report sent to %s -> HTTP Status %d", targetURL, resp.StatusCode)
}