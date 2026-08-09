//go:build windows
// +build windows

package main

import (
	"log"

	"golang.org/x/sys/windows/registry"
)

type Config struct {
	BaseURL         string
	WorkspaceSlug   string
	ReportSecret    string
	ReportBitLocker bool
	ReportFirewall  bool
	ReportApps      bool
	IntervalSec     int
}

func LoadConfig() Config {
	config := Config{
		BaseURL:         "https://soar.mi-labs.es",
		WorkspaceSlug:   "friendly-emporium",
		ReportSecret:    "db4rLzdlJBo08SArnnH9pHZm",
		ReportBitLocker: true,
		ReportFirewall:  true,
		ReportApps:      false,
		IntervalSec:     3600,
	}

	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Policies\Applivery\SOAR`, registry.QUERY_VALUE)
	if err != nil {
		log.Println("No Managed Configuration found in Registry. Using default settings.")
		return config
	}
	defer k.Close()

	if val, _, err := k.GetStringValue("BaseURL"); err == nil && val != "" {
		config.BaseURL = val
	}
	if val, _, err := k.GetStringValue("WorkspaceSlug"); err == nil && val != "" {
		config.WorkspaceSlug = val
	}
	if val, _, err := k.GetStringValue("ReportSecret"); err == nil && val != "" {
		config.ReportSecret = val
	}
	
	if val, _, err := k.GetIntegerValue("ReportBitLocker"); err == nil {
		config.ReportBitLocker = (val == 1)
	}
	if val, _, err := k.GetIntegerValue("ReportFirewall"); err == nil {
		config.ReportFirewall = (val == 1)
	}
	if val, _, err := k.GetIntegerValue("ReportApps"); err == nil {
		config.ReportApps = (val == 1)
	}
	if val, _, err := k.GetIntegerValue("IntervalSec"); err == nil && val > 0 {
		config.IntervalSec = int(val)
	}

	return config
}