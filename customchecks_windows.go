//go:build windows
// +build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Custom Device Checks — admin-defined queries authored in Settings > Custom
// Device Checks (backend's customChecks.service.ts has the full design).
// This agent polls GET /api/device-data/custom-checks?platform=windows once
// per report cycle, runs every enabled check it gets back locally, and
// includes the results in the SAME POST /api/device-data/report call
// telemetry_windows.go already makes (customCheckResults field) — no
// separate report call, no local persistence between cycles.
//
// CustomCheckResult.Error is set ONLY when the check itself failed to run
// (WMI query error, registry/service not found, command timeout) — a
// legitimately negative result (process not running, service stopped) is a
// normal Value, not an Error. The backend's compliance evaluator treats an
// errored result the same as "missing" (complianceEvaluate.ts).

type CustomCheckDef struct {
	Key         string                 `json:"key"`
	CheckerType string                 `json:"checkerType"`
	Params      map[string]interface{} `json:"params"`
}

type CustomCheckResult struct {
	Value interface{} `json:"value,omitempty"`
	Error string      `json:"error,omitempty"`
}

func fetchCustomChecks(baseURL *url.URL, config Config) []CustomCheckDef {
	checksURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/custom-checks", RawQuery: "platform=windows"}).String()

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", checksURL, nil)
	if err != nil {
		log.Printf("Error building custom-checks poll request: %v", err)
		return nil
	}
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
	req.Header.Set("X-Device-Report-Secret", config.ReportSecret)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error polling custom checks: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Custom checks poll returned HTTP %d — skipping this cycle's custom checks", resp.StatusCode)
		return nil
	}

	var body struct {
		Items []CustomCheckDef `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("Error decoding custom checks response: %v", err)
		return nil
	}
	return body.Items
}

func runCustomChecks(checks []CustomCheckDef) map[string]CustomCheckResult {
	if len(checks) == 0 {
		return nil
	}
	results := make(map[string]CustomCheckResult, len(checks))
	for _, c := range checks {
		results[c.Key] = runOneCustomCheck(c)
	}
	return results
}

func runOneCustomCheck(c CustomCheckDef) CustomCheckResult {
	switch c.CheckerType {
	case "processRunning":
		name, _ := c.Params["processName"].(string)
		running, err := isProcessRunning(name)
		if err != nil {
			return CustomCheckResult{Error: err.Error()}
		}
		return CustomCheckResult{Value: running}

	case "serviceStatus":
		name, _ := c.Params["serviceName"].(string)
		running, err := isServiceRunning(name)
		if err != nil {
			return CustomCheckResult{Error: err.Error()}
		}
		return CustomCheckResult{Value: running}

	case "registryOrFileValue":
		path, _ := c.Params["registryPath"].(string)
		valueName, _ := c.Params["valueName"].(string)
		val, err := readRegistryValue(path, valueName)
		if err != nil {
			return CustomCheckResult{Error: err.Error()}
		}
		return CustomCheckResult{Value: val}

	case "appInstalled":
		identifier, _ := c.Params["identifier"].(string)
		version, err := findInstalledAppVersion(identifier)
		if err != nil {
			return CustomCheckResult{Error: err.Error()}
		}
		return CustomCheckResult{Value: version}

	case "command":
		command, _ := c.Params["command"].(string)
		out, err := runCustomCommand(command)
		if err != nil {
			return CustomCheckResult{Error: err.Error()}
		}
		return CustomCheckResult{Value: out}

	default:
		return CustomCheckResult{Error: fmt.Sprintf("unknown checker type %q", c.CheckerType)}
	}
}

type Win32_Process struct {
	Name string
}

// isProcessRunning returns (false, nil) — not an error — when the process
// simply isn't running; an error return means the WMI query itself failed.
func isProcessRunning(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("no process name configured")
	}
	var dst []Win32_Process
	query := wmi.CreateQuery(&dst, "")
	if err := wmi.Query(query, &dst); err != nil {
		return false, fmt.Errorf("querying processes: %v", err)
	}
	target := strings.ToLower(name)
	for _, p := range dst {
		if strings.ToLower(p.Name) == target {
			return true, nil
		}
	}
	return false, nil
}

// isServiceRunning treats "service not installed" as an error (can't
// determine state), distinct from "installed but stopped" (a normal false).
func isServiceRunning(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("no service name configured")
	}
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("connecting to Service Control Manager: %v", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return false, fmt.Errorf("service %q not found", name)
	}
	defer s.Close()
	status, err := s.Query()
	if err != nil {
		return false, fmt.Errorf("querying service %q: %v", name, err)
	}
	return status.State == svc.Running, nil
}

// readRegistryValue only supports HKLM/HKCU (matching customChecks.schemas.ts's
// validateCheckParams doc comment) — reads a string value directly, or falls
// back to an integer value formatted as a decimal string.
func readRegistryValue(path, valueName string) (string, error) {
	path = strings.TrimSpace(path)
	valueName = strings.TrimSpace(valueName)
	if path == "" || valueName == "" {
		return "", fmt.Errorf("registry path and value name are required")
	}
	parts := strings.SplitN(path, `\`, 2)
	if len(parts) != 2 {
		return "", fmt.Errorf(`registry path must include a subkey, e.g. HKLM\SOFTWARE\Vendor`)
	}
	var hive registry.Key
	switch strings.ToUpper(parts[0]) {
	case "HKLM", "HKEY_LOCAL_MACHINE":
		hive = registry.LOCAL_MACHINE
	case "HKCU", "HKEY_CURRENT_USER":
		hive = registry.CURRENT_USER
	default:
		return "", fmt.Errorf("unsupported registry hive %q — only HKLM and HKCU are supported", parts[0])
	}
	k, err := registry.OpenKey(hive, parts[1], registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("registry key not found: %v", err)
	}
	defer k.Close()
	if s, _, err := k.GetStringValue(valueName); err == nil {
		return s, nil
	}
	if i, _, err := k.GetIntegerValue(valueName); err == nil {
		return strconv.FormatUint(i, 10), nil
	}
	return "", fmt.Errorf("value %q not found under %s (or isn't a string/integer)", valueName, path)
}

// findInstalledAppVersion reuses the same winget/registry inventory
// apps_windows.go's GetInstalledApps() already builds for app-inventory
// reporting — no separate lookup path to keep in sync.
func findInstalledAppVersion(identifier string) (string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", fmt.Errorf("no app identifier configured")
	}
	target := strings.ToLower(identifier)
	for _, app := range GetInstalledApps() {
		if strings.ToLower(app.Identifier) == target {
			if app.Version != "" {
				return app.Version, nil
			}
			return "installed", nil
		}
	}
	return "", fmt.Errorf("app %q not found", identifier)
}

// runCustomCommand is the "advanced" checker type — see this repo's README
// and Settings > Custom Device Checks' UI warning: it runs exactly what the
// admin entered, with no sandboxing beyond the agent's own process
// privileges. 30s timeout, output capped at 4KB before being reported.
func runCustomCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("no command configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	output := strings.TrimSpace(out.String())
	if len(output) > 4000 {
		output = output[:4000] + "… (truncated)"
	}

	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("command timed out after 30s")
	}
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("command exited with error: %v — output: %s", err, output)
		}
		return "", fmt.Errorf("command exited with error: %v", err)
	}
	return output, nil
}
