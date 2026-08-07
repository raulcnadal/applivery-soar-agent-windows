//go:build windows
// +build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
)

// DeviceData matches the JSON payload expected by your SOAR webhook
type DeviceData struct {
	Platform     string                 `json:"platform"`
	SerialNumber string                 `json:"serialNumber"`
	Attributes   map[string]interface{} `json:"attributes"`
}

func main() {
	fmt.Println("Starting Applivery SOAR Windows Agent...")

	// TODO 1: Read UEM configuration from HKLM\SOFTWARE\Policies...
	// TODO 2: Query WMI for BitLockerStatus, FirewallEnabled, and OsBuild
	// TODO 3: Construct the JSON payload
	// TODO 4: POST to https://soar.mi-labs.es/api/device-data/report with headers
}