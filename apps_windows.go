//go:build windows
// +build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

type InstalledApp struct {
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	// Windows-only, optional — "winget" for apps detected via `winget list`
	// (a real package-manager source, not just "some installer ran"), "msi"
	// for the registry Uninstall-key fallback (used only when winget itself
	// isn't invokable — see GetInstalledApps' doc comment), "store" for
	// AppX/UWP packages (PowerShell Get-AppxPackage-sourced). Split into
	// "winget" vs "msi" specifically so SOAR's Apps view can show a genuine
	// "Winget" Source value instead of collapsing every self-reported Win32
	// app into one undifferentiated bucket — a real user-requested
	// distinction (Winget / MS Store / UEM / Manual). Omitted (empty)
	// rather than defaulted so an older backend that doesn't know this
	// field yet just sees it absent, same as any other optional JSON
	// field. Mirrors the backend's own origin convention for server-fetched
	// apps — see SOAR's installedApps.service.ts InstalledAppsEntry.apps[].origin
	// doc comment.
	Origin string `json:"origin,omitempty"`
	// InstallLocation is the on-disk install path, when known — for AppX/
	// Store packages this is always available (Get-AppxPackage's own
	// InstallLocation property, e.g. "C:\Program Files\WindowsApps\
	// 1ED5AEA5.4160926B82DB_3.17.10.0_x64__p2gbknwb5d8r2"); for classic
	// Win32 installs found via the registry it's the Uninstall key's own
	// "InstallLocation" value when the installer bothered to write one
	// (many don't). Never populated for winget-sourced entries — `winget
	// list` doesn't surface it without a second per-package query this
	// agent doesn't make. Purely informational (surfaced in SOAR's App
	// detail modal); never used for identifier/matching logic.
	InstallLocation string `json:"installLocation,omitempty"`
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
//
// Neither winget nor the registry's Uninstall keys ever surface AppX/Store
// packages (Calculator, Store-installed apps, etc.) — those aren't
// classic Win32 installs and don't register themselves either way, which is
// exactly the "142 apps in Applivery UEM vs 9 self-reported" gap this
// function now closes. getAppsViaAppx (PowerShell Get-AppxPackage -AllUsers,
// which — unlike winget.exe — works fine under the LocalSystem account this
// agent normally runs as) is appended unconditionally alongside whichever
// Win32 source above succeeded, tagged Origin:"store" so the backend can
// still tell the two kinds apart (mirrors the "msi"/"store" split
// installedApps.service.ts already applies to Applivery UEM's own fetch).
// Identifier collisions keep the Win32 entry, matching the backend's own
// dedup precedence in fetchAndStoreInstalledApps's Windows branch.
func GetInstalledApps() []InstalledApp {
	var apps []InstalledApp
	if winget := getAppsViaWinget(); len(winget) > 0 {
		apps = winget
	} else {
		apps = getAppsViaRegistry()
	}

	seen := make(map[string]bool, len(apps))
	for _, a := range apps {
		seen[strings.ToLower(a.Identifier)] = true
	}
	for _, a := range getAppsViaAppx() {
		id := strings.ToLower(a.Identifier)
		if seen[id] {
			continue
		}
		seen[id] = true
		apps = append(apps, a)
	}
	return apps
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
		apps = append(apps, InstalledApp{Identifier: id, Name: values["Name"], Version: values["Version"], Origin: "winget"})
	}
	return apps
}

// getAppsViaRegistry only runs when winget itself isn't usable (see
// GetInstalledApps), so everything it finds is tagged Origin:"msi" — a
// best-effort "this is some classic Win32 install, but we didn't get it via
// a real package manager" signal, distinct from "winget" above.
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
			installLocation, _, _ := subKey.GetStringValue("InstallLocation")
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
				Identifier:      identifier,
				Name:            displayName,
				Version:         displayVersion,
				Origin:          "msi",
				InstallLocation: strings.TrimSpace(installLocation),
			})
		}
	}

	return apps
}

// getAppsViaAppx enumerates AppX/UWP packages (Store apps, and Windows' own
// bundled system apps like Calculator/Photos/Store itself) via PowerShell's
// Get-AppxPackage -AllUsers. This is the piece winget/registry structurally
// can't see — neither surfaces AppX packages at all, since they don't
// register under the classic Uninstall registry keys and winget only lists
// Win32-style installs — and is what actually closes the "142 apps visible
// in Applivery UEM vs 9 self-reported by this agent" gap: those missing
// ~130 apps were Store/system packages this agent never looked for.
// -AllUsers (not the user-scoped default) is required to see packages
// installed for the interactively logged-on user while running as
// LocalSystem, which is how this agent's Windows Service normally runs —
// unlike winget.exe, Get-AppxPackage works fine under LocalSystem.
// IsFramework/IsResourcePackage packages are excluded: they're not
// standalone apps (shared runtime libraries and language resource packs,
// respectively), so listing them would just be noise no admin would ever
// build an App List entry against — same IsFramework exclusion
// windowsDeviceApps.service.ts applies to Applivery UEM's own
// AppInventoryResults parse, for the same reason.
//
// $apps is wrapped in @(...) and passed via -InputObject (not the pipeline)
// specifically so ConvertTo-Json always emits a JSON array — piping an
// array into ConvertTo-Json instead enumerates and flattens it, so a device
// with exactly one matching package would otherwise serialize as a bare
// JSON object our decoder can't parse as a list, and zero matches would
// print nothing at all rather than "[]".
//
// Display-name resolution: a package's real, human-friendly name isn't
// exposed by Get-AppxPackage at all — its own `Name` property is the
// package IDENTITY (e.g. "1ED5AEA5.4160926B82DB" for a Store-installed
// game), not something meant for display. The actual name lives in
// AppxManifest.xml's <Properties><DisplayName>, which is *sometimes* an
// indirect resource reference ("ms-resource:AppName" or similar) requiring
// the package's own resources.pri to resolve, but is very often already a
// literal string (confirmed live: Angry Birds 2's own manifest DisplayName
// is the literal text "Angry Birds 2", no indirection at all).
//
// An earlier version of this function read the manifest via the
// `Get-AppxPackageManifest` cmdlet. That never actually worked under this
// agent's LocalSystem Windows Service context — the cmdlet silently failed
// on every package (caught by its own try/catch, so the failure was
// invisible), meaning DisplayName resolution never ran at all, for *any*
// package, resource-indirected or not. Confirmed by direct comparison: a
// plain `Get-Content "$($pkg.InstallLocation)\AppxManifest.xml"` read of the
// exact same package, on the same machine, returns the manifest fine.
// `Get-AppxPackageManifest` goes through the AppX deployment/WinRT stack,
// which — unlike Get-AppxPackage's own enumeration — appears to expect an
// interactive user session; reading the manifest file directly off disk via
// InstallLocation (plain filesystem access, no WinRT/deployment API
// involved) sidesteps that entirely, so that's what this function does now.
//
// For each package: read InstallLocation\AppxManifest.xml straight off
// disk, parse Properties/DisplayName. If it's a literal string, use it
// as-is. If it's an "ms-resource:" indirect reference, resolve it via a
// P/Invoke to shlwapi.dll's SHLoadIndirectString (a plain Win32 resource-
// loading API, not a WinRT/shell-integration one, so — unlike Get-StartApps
// — it doesn't need an interactive session either) using the documented
// "@{PackageFullName?<raw ms-resource value>}" indirect-string form. Every
// step is wrapped in try/catch and silently falls back to the previous
// behavior (raw identity name as the display name) on any failure — a
// resolution miss should never cost a device its app-inventory data.
// InstallLocation itself is also reported (purely informational, shown in
// SOAR's App detail modal). `Identifier` is unaffected by any of this: it
// stays the lowercased package identity name exactly as before, so it keeps
// agreeing with Applivery UEM's own AppInventoryResults-sourced identifier
// (windowsDeviceApps.service.ts) — only the display Name changes.
func getAppsViaAppx() []InstalledApp {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	script := `
Add-Type -TypeDefinition @"
using System;
using System.Text;
using System.Runtime.InteropServices;
public class ApplveryAppxRes {
    [DllImport("shlwapi.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern int SHLoadIndirectString(string pszSource, StringBuilder pszOutBuf, int cchOutBuf, IntPtr ppvReserved);
}
"@ -ErrorAction SilentlyContinue

$pkgs = @(Get-AppxPackage -AllUsers -ErrorAction SilentlyContinue | Where-Object { -not $_.IsFramework -and -not $_.IsResourcePackage })
$apps = @($pkgs | ForEach-Object {
    $displayName = $null
    $installLocation = $_.InstallLocation
    try {
        if ($installLocation) {
            $manifestPath = Join-Path $installLocation "AppxManifest.xml"
            if (Test-Path -LiteralPath $manifestPath) {
                [xml]$manifest = Get-Content -LiteralPath $manifestPath -ErrorAction Stop
                $raw = $manifest.Package.Properties.DisplayName
                if ($raw) {
                    if ($raw.ToString().StartsWith('ms-resource:')) {
                        try {
                            $buf = New-Object System.Text.StringBuilder 1024
                            $indirect = "@{$($_.PackageFullName)?$raw}"
                            $hr = [ApplveryAppxRes]::SHLoadIndirectString($indirect, $buf, $buf.Capacity, [IntPtr]::Zero)
                            if ($hr -eq 0 -and $buf.Length -gt 0) { $displayName = $buf.ToString() }
                        } catch {}
                    } else {
                        $displayName = $raw.ToString()
                    }
                }
            }
        }
    } catch {}
    [PSCustomObject]@{ Name = $_.Name; Version = $_.Version; DisplayName = $displayName; InstallLocation = $installLocation }
})
ConvertTo-Json -InputObject $apps -Compress
`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("Get-AppxPackage enumeration timed out after 90s (Store/system apps will be missing from this report)")
		} else {
			log.Printf("Get-AppxPackage enumeration failed (Store/system apps will be missing from this report): %v", err)
		}
		return nil
	}

	var raw []struct {
		Name            string `json:"Name"`
		Version         string `json:"Version"`
		DisplayName     string `json:"DisplayName"`
		InstallLocation string `json:"InstallLocation"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &raw); err != nil {
		log.Printf("Could not parse Get-AppxPackage output (Store/system apps will be missing from this report): %v", err)
		return nil
	}

	// Identifier stays the lowercased package identity Name (e.g.
	// "microsoft.windowscalculator") exactly as before — see this function's
	// own doc comment for why that's deliberately unchanged. Name prefers
	// the resolved DisplayName; falls back to the identity name (today's
	// behavior) whenever resolution didn't produce anything usable — an
	// empty result, or (defensively) a string that still looks like an
	// unresolved resource reference.
	var apps []InstalledApp
	seen := make(map[string]bool, len(raw))
	for _, a := range raw {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		identifier := strings.ToLower(name)
		if seen[identifier] {
			continue
		}
		seen[identifier] = true
		displayName := strings.TrimSpace(a.DisplayName)
		if displayName == "" || strings.HasPrefix(strings.ToLower(displayName), "ms-resource") {
			displayName = name
		}
		apps = append(apps, InstalledApp{
			Identifier:      identifier,
			Name:            displayName,
			Version:         strings.TrimSpace(a.Version),
			Origin:          "store",
			InstallLocation: strings.TrimSpace(a.InstallLocation),
		})
	}
	return apps
}