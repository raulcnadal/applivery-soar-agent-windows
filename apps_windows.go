//go:build windows
// +build windows

package main

import (
	"bufio"
	"bytes"
	"log"
	"os/exec"
	"regexp"
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

// GetInstalledApps prefers `winget list`, matching report-installed-apps.ps1
// (Settings > Device Data Webhook): winget's PackageIdentifier (e.g.
// "Mozilla.Firefox") is exactly what App Lists' Winget search source
// returns, so a report from here matches those entries precisely — the
// registry Uninstall-key fallback below has no cross-vendor stable
// identifier, so its "identifier" is just the lowercased DisplayName, only
// useful for App List entries added manually under a matching name. Only
// falls back to the registry if winget.exe isn't on PATH or its output
// didn't parse (e.g. running as LocalSystem, where winget/App Installer
// — a per-user MSIX package — isn't always present or invokable; this is a
// known Windows limitation, not something this agent can work around).
func GetInstalledApps() []InstalledApp {
	if apps := getAppsViaWinget(); len(apps) > 0 {
		return apps
	}
	return getAppsViaRegistry()
}

var wingetHeaderRe = regexp.MustCompile(`^Name\s+Id\s+Version`)

func getAppsViaWinget() []InstalledApp {
	if _, err := exec.LookPath("winget.exe"); err != nil {
		return nil
	}

	cmd := exec.Command("winget.exe", "list", "--accept-source-agreements")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		log.Printf("winget list failed, falling back to registry enumeration: %v", err)
		return nil
	}

	var headerLine string
	var dataLines []string
	scanner := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if headerLine == "" && wingetHeaderRe.MatchString(line) {
			headerLine = line
			continue
		}
		if headerLine == "" || line == "" || strings.Trim(line, "- ") == "" {
			continue
		}
		dataLines = append(dataLines, line)
	}
	if headerLine == "" {
		return nil
	}

	// winget's `list` output is a fixed-width table with no built-in
	// machine-readable format in most currently-deployed versions, so every
	// column's start position is located from the header row instead of
	// hardcoded indexes (they shift with locale/version, and the trailing
	// "Available"/"Source" columns aren't always present) — same approach
	// as report-installed-apps.ps1.
	type col struct {
		name  string
		start int
	}
	var cols []col
	for _, name := range []string{"Name", "Id", "Version"} {
		if idx := strings.Index(headerLine, name); idx >= 0 {
			cols = append(cols, col{name, idx})
		}
	}
	if len(cols) < 3 {
		return nil
	}

	var apps []InstalledApp
	seen := make(map[string]bool)
	for _, line := range dataLines {
		values := make(map[string]string)
		for i, c := range cols {
			end := len(line)
			if i+1 < len(cols) && cols[i+1].start < end {
				end = cols[i+1].start
			}
			if c.start >= len(line) {
				continue
			}
			values[c.name] = strings.TrimSpace(line[c.start:min(end, len(line))])
		}
		id := values["Id"]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		apps = append(apps, InstalledApp{Identifier: id, Name: values["Name"], Version: values["Version"]})
	}
	return apps
}

func getAppsViaRegistry() []InstalledApp {
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