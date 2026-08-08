//go:build windows
// +build windows

package main

import (
	"log"
	"strings"

	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
)

// --- Structs for JSON Payloads ---

type App struct {
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
	Version    string `json:"version"`
}

type AppsPayload struct {
	Platform     string `json:"platform"`
	SerialNumber string `json:"serialNumber"`
	Apps         []App  `json:"apps"`
}

// --- Structs for WMI Queries ---

type Win32_BIOS struct {
	SerialNumber string
}

type Win32_EncryptableVolume struct {
	ProtectionStatus uint32
}

// --- Data Gathering Functions ---

func GetSerialNumber() string {
	var dst []Win32_BIOS
	q := wmi.CreateQuery(&dst, "")
	err := wmi.Query(q, &dst)
	if err != nil || len(dst) == 0 {
		log.Printf("Failed to get Serial Number via WMI: %v", err)
		return "UNKNOWN"
	}
	return strings.TrimSpace(dst[0].SerialNumber)
}

func GetOSBuild() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		log.Printf("Failed to open Registry for OS Build: %v", err)
		return "UNKNOWN"
	}
	defer k.Close()

	build, _, err := k.GetStringValue("CurrentBuild")
	if err != nil {
		return "UNKNOWN"
	}
	return build
}

func GetBitLockerStatus() bool {
	var dst []Win32_EncryptableVolume
	// BitLocker requires querying a specific WMI namespace, just like your PS1 script
	q := wmi.CreateQuery(&dst, "")
	err := wmi.QueryNamespace(q, &dst, `root\CIMv2\Security\MicrosoftVolumeEncryption`)
	
	if err != nil || len(dst) == 0 {
		log.Printf("Failed to query BitLocker WMI namespace (Requires Admin): %v", err)
		return false
	}
	
	// ProtectionStatus 1 means ON
	return dst[0].ProtectionStatus == 1
}

func GetFirewallStatus() bool {
	// Reads the Domain/Standard profile directly from the Registry
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\SharedAccess\Parameters\FirewallPolicy\StandardProfile`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()

	val, _, err := k.GetIntegerValue("EnableFirewall")
	if err != nil {
		return false
	}
	
	// 1 means Enabled
	return val == 1
}

func GetInstalledApps() []App {
	var apps []App
	
	// Replicating the fallback logic from your apps-windows.ps1
	paths := []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	}

	for _, path := range paths {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		
		subkeys, err := k.ReadSubKeyNames(-1)
		k.Close()
		if err != nil {
			continue
		}

		for _, subkey := range subkeys {
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, path+`\`+subkey, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			
			name, _, err := sk.GetStringValue("DisplayName")
			if err == nil && name != "" {
				version, _, _ := sk.GetStringValue("DisplayVersion")
				apps = append(apps, App{
					Identifier: strings.ToLower(name), 
					Name:       name,
					Version:    version,
				})
			}
			sk.Close()
		}
	}
	return apps
}