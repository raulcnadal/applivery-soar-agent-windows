//go:build windows
// +build windows

package main

import (
	"log"
	"github.com/yusufpapurcu/wmi"
)

// Win32_OperatingSystem maps to the standard WMI OS class
type Win32_OperatingSystem struct {
	BuildNumber string
}

// Win32_EncryptableVolume maps to the BitLocker class
type Win32_EncryptableVolume struct {
	ProtectionStatus uint32
}

// FirewallProduct maps to the Windows Security Center firewall class
type FirewallProduct struct {
	displayName string
}

// GetOSBuild queries WMI for the Windows OS Build number
func GetOSBuild() string {
	var dst []Win32_OperatingSystem
	
	// Constructs: SELECT BuildNumber FROM Win32_OperatingSystem
	query := wmi.CreateQuery(&dst, "")
	err := wmi.Query(query, &dst)
	if err != nil || len(dst) == 0 {
		log.Printf("Error querying OS Build: %v", err)
		return "Unknown"
	}
	
	return dst[0].BuildNumber
}

// GetBitLockerStatus queries WMI for BitLocker protection status
// NOTE: This requires the agent to be running with Administrator privileges!
func GetBitLockerStatus() bool {
	var dst []Win32_EncryptableVolume
	
	query := wmi.CreateQuery(&dst, "")
	// BitLocker data lives in a specific namespace, not the default CIMv2
	err := wmi.QueryNamespace(query, &dst, `root\CIMv2\Security\MicrosoftVolumeEncryption`)
	
	if err != nil || len(dst) == 0 {
		log.Printf("Error querying BitLocker (Check Admin Privileges): %v", err)
		return false
	}
	
	// ProtectionStatus: 0 = Off, 1 = On, 2 = Unknown
	// We check if at least the OS drive (usually index 0) is protected
	return dst[0].ProtectionStatus == 1
}

// GetFirewallStatus queries the Security Center to see if a Firewall is registered
func GetFirewallStatus() bool {
	var dst []FirewallProduct
	query := wmi.CreateQuery(&dst, "")
	
	// Security products live in SecurityCenter2
	err := wmi.QueryNamespace(query, &dst, `root\SecurityCenter2`)
	if err != nil {
		log.Printf("Error querying Firewall Status: %v", err)
		return false
	}
	
	// If the array has at least one item, a firewall is registered and active
	return len(dst) > 0
}