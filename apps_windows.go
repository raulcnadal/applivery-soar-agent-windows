//go:build windows
// +build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

type InstalledApp struct {
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
	Version    string `json:"version"`
}

type AppsPayload struct {
	Platform     string         `json:"platform"`
	SerialNumber string         `json:"serialNumber"`
	Apps         []InstalledApp `json:"apps"`
}

func GetInstalledApps() []InstalledApp {
	var apps []InstalledApp
	seen := make(map[string]bool)

	registryPaths := []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	}

	for _, target := range registryPaths {
		key, err := registry.OpenKey(target.root, target.path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
		if err != nil {
			continue
		}

		subKeyNames, err := key.ReadSubKeyNames(-1)
		key.Close()
		if err != nil {
			continue
		}

		for _, subName := range subKeyNames {
			subKey, err := registry.OpenKey(target.root, target.path+`\`+subName, registry.QUERY_VALUE)
			if err != nil {
				continue
			}

			displayName, _, errName := subKey.GetStringValue("DisplayName")
			displayVersion, _, _ := subKey.GetStringValue("DisplayVersion")
			systemComponent, _, _ := subKey.GetIntegerValue("SystemComponent")
			subKey.Close()

			if errName != nil || strings.TrimSpace(displayName) == "" || systemComponent == 1 {
				continue
			}

			displayName = strings.TrimSpace(displayName)
			identifier := strings.ToLower(displayName)

			if seen[identifier] {
				continue
			}
			seen[identifier] = true

			apps = append(apps, InstalledApp{
				Identifier: identifier,
				Name:       displayName,
				Version:    displayVersion,
			})
		}
	}

	return apps
}