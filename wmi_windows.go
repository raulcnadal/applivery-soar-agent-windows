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

// GetFirewallStatus checks all three Windows Firewall profiles — Domain,
// Standard (a.k.a. Private), and Public — not just Standard. An earlier
// version of this function only read StandardProfile, so a domain-joined
// machine with its Domain profile firewall off (while Private/Public stayed
// on, the common default) would still report FirewallEnabled=true. Windows
// itself only ever has ONE profile active at a time based on the network's
// detected category, and there's no single WMI/registry read that reports
// "which profile is active right now" as reliably as just checking every
// profile that exists — so this reports true only if every profile present
// has its firewall on; a profile key that doesn't exist is treated as
// enabled, matching Windows' own default (all three profiles ship
// enabled unless an admin explicitly turned one off).
func GetFirewallStatus() bool {
	profiles := []string{"DomainProfile", "StandardProfile", "PublicProfile"}
	for _, profile := range profiles {
		path := `SYSTEM\CurrentControlSet\Services\SharedAccess\Parameters\FirewallPolicy\` + profile
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			// Key absent — not the same as "disabled"; Windows treats an
			// unconfigured profile as enabled (its own shipped default).
			continue
		}
		val, _, err := k.GetIntegerValue("EnableFirewall")
		k.Close()
		if err == nil && val == 0 {
			return false
		}
	}
	return true
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