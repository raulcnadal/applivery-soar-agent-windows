//go:build windows
// +build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// DeviceData matches the JSON payload expected by your SOAR webhook
type DeviceData struct {
	Platform     string                 `json:"platform"`
	SerialNumber string                 `json:"serialNumber"`
	Attributes   map[string]interface{} `json:"attributes"`
}

func main() {
	fmt.Println("Starting Applivery SOAR Windows Agent...")

	// 1. Read UEM configuration from HKLM\SOFTWARE\Policies
	config := LoadConfig()

	// 2. Gather WMI data based on UEM configuration preferences
	attributes := make(map[string]interface{})

	if config.ReportBitLocker {
		attributes["BitLockerStatus"] = GetBitLockerStatus()
	}
	
	if config.ReportFirewall {
		attributes["FirewallEnabled"] = GetFirewallStatus()
	}
	
	// We will always report OS Build for this example
	attributes["OsBuild"] = GetOSBuild()

	// 3. Construct the JSON payload
	payload := DeviceData{
		Platform:     "windows",
		SerialNumber: GetSerialNumber(),
		Attributes:   attributes,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Fatalf("Error marshaling JSON: %v", err)
	}

	// 4. POST to https://soar.mi-labs.es/api/device-data/report
	webhookURL := "https://soar.mi-labs.es/api/device-data/report"
	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Fatalf("Error creating HTTP request: %v", err)
	}

	// Append necessary headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
	
	// Note: For absolute security, this secret could also be pushed via the UEM registry config 
	// rather than hardcoded, but we will use your example secret here.
	req.Header.Set("X-Device-Report-Secret", "db4rLzdlJBo08SArnnH9pHZm")

	// 5. Execute Request
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("Failed to send report to webhook: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("Successfully sent device data! Server responded with status code: %d", resp.StatusCode)
}