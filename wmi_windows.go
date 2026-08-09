//go:build windows
// +build windows

package main

import (
	"log"
	"strings"

	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
)

type Win32_OperatingSystem struct {
	BuildNumber string
}

type Win32_EncryptableVolume struct {
	ProtectionStatus uint32
}

type FirewallProduct struct {
	displayName string
}

type Win32_BIOS struct {
	SerialNumber string
}

func GetOSBuild() string {
	var dst []Win32_OperatingSystem
	query := wmi.CreateQuery(&dst, "")
	err := wmi.Query(query, &dst)
	if err != nil || len(dst) == 0 {
		log.Printf("Error querying OS Build: %v", err)
		return "Unknown"
	}
	return dst[0].BuildNumber
}

func GetBitLockerStatus() bool {
	var dst []Win32_EncryptableVolume
	query := wmi.CreateQuery(&dst, "")
	err := wmi.QueryNamespace(query, &dst, `root\CIMv2\Security\MicrosoftVolumeEncryption`)
	if err != nil || len(dst) == 0 {
		log.Printf("Error querying BitLocker: %v", err)
		return false
	}
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
	return val == 1
}

func GetSerialNumber() string {
	var dst []Win32_BIOS
	query := wmi.CreateQuery(&dst, "")
	err := wmi.Query(query, &dst)
	if err != nil || len(dst) == 0 {
		log.Printf("Failed to get Serial Number via WMI: %v", err)
		return "UNKNOWN"
	}
	return strings.TrimSpace(dst[0].SerialNumber)
}