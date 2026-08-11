//go:build windows
// +build windows

// The Applivery SOAR Tray helper — a small, unprivileged, per-user-session
// process that shows a persistent notification-area icon so a user always
// has a visible, informational signal that device management/compliance
// reporting is active, plus a right-click summary of what's being reported
// and this device's current Compliance Policy status. It is deliberately
// NOT a Windows service: services run in Session 0, which has had no
// desktop/UI access since Windows Vista's session isolation, so a service
// can never show a tray icon in an interactive user's session directly.
// Instead the installer registers a Scheduled Task (agent.wxs) that starts
// this exe once any user logs on.
//
// This process is read-only: it has no registry access to the Managed
// Configuration secret and no HTTP client of its own. Everything it shows
// comes from %ProgramData%\Applivery\SOAR\status.json, written by the main
// AppliverySOARAgent service after every report cycle (status_windows.go /
// internal/agentstatus in the repo root) — that package's doc comment has
// the full rationale for the split. Re-read fresh every time the user
// right-clicks (and on a 60s timer for the tooltip/icon), so the menu is
// never more than one report cycle stale.
//
// Built entirely on raw syscalls against user32.dll/shell32.dll via the
// standard library's syscall package — no GUI toolkit dependency, matching
// this repo's existing style (wmi_windows.go, registry_windows.go) of
// hand-rolled Win32 calls over a heavier abstraction, and avoiding a new
// third-party dependency this repo's build can't verify offline.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"github.com/raulcnadal/applivery-soar-agent-windows/internal/agentlog"
	"github.com/raulcnadal/applivery-soar-agent-windows/internal/agentstatus"
)

//go:embed icons/tray_light.ico icons/tray_dark.ico
var iconFS embed.FS

const (
	wmDestroy       = 0x0002
	wmSettingChange = 0x001A
	wmTimer         = 0x0113
	wmLButtonUp     = 0x0202
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	wmTrayIcon      = wmApp + 1

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	imageIcon      = 1
	lrLoadFromFile = 0x00000010

	smCxSmIcon = 49
	smCySmIcon = 50

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfDisabled  = 0x00000002
	mfSeparator = 0x00000800

	tpmRightAlign  = 0x0008
	tpmBottomAlign = 0x0020
	tpmReturnCmd   = 0x0100

	swShowNormal = 1

	cmdInfo           = 9000
	cmdOpenDashboard  = 1001
	cmdExit           = 1002
	trayIconID        = 1
	refreshTimerID    = 1
	refreshIntervalMs = 60_000
)

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")
	moduser32   = syscall.NewLazyDLL("user32.dll")
	modshell32  = syscall.NewLazyDLL("shell32.dll")

	procGetModuleHandleW = modkernel32.NewProc("GetModuleHandleW")

	procRegisterClassExW = moduser32.NewProc("RegisterClassExW")
	procCreateWindowExW  = moduser32.NewProc("CreateWindowExW")
	procDefWindowProcW   = moduser32.NewProc("DefWindowProcW")
	procDestroyWindow    = moduser32.NewProc("DestroyWindow")
	procPostQuitMessage  = moduser32.NewProc("PostQuitMessage")
	procGetMessageW      = moduser32.NewProc("GetMessageW")
	procTranslateMessage = moduser32.NewProc("TranslateMessage")
	procDispatchMessageW = moduser32.NewProc("DispatchMessageW")
	procLoadImageW       = moduser32.NewProc("LoadImageW")
	procDestroyIcon      = moduser32.NewProc("DestroyIcon")
	procCreatePopupMenu  = moduser32.NewProc("CreatePopupMenu")
	procDestroyMenu      = moduser32.NewProc("DestroyMenu")
	procAppendMenuW      = moduser32.NewProc("AppendMenuW")
	procTrackPopupMenuEx = moduser32.NewProc("TrackPopupMenuEx")
	procSetForegroundWin = moduser32.NewProc("SetForegroundWindow")
	procPostMessageW     = moduser32.NewProc("PostMessageW")
	procGetCursorPos     = moduser32.NewProc("GetCursorPos")
	procGetSystemMetrics = moduser32.NewProc("GetSystemMetrics")
	procSetTimer         = moduser32.NewProc("SetTimer")
	procKillTimer        = moduser32.NewProc("KillTimer")

	procShellNotifyIconW = modshell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW    = modshell32.NewProc("ShellExecuteW")
)

type point struct{ x, y int32 }

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// guid mirrors the Win32 GUID struct — needed only because notifyIconData
// (below) includes one as a trailing, always-zeroed field; we never
// populate it ourselves.
type guid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

// notifyIconData mirrors the full, modern NOTIFYICONDATAW struct (the one
// MSDN itself recommends every Vista+ caller use, cbSize =
// sizeof(NOTIFYICONDATAW), rather than one of the older partial-compatibility
// sizes) — Shell_NotifyIconW's version-detection logic keys off cbSize
// matching one of a few recognized checkpoints, and sizeof(the full struct)
// is the one guaranteed to be recognized on every version this agent
// targets. Every field past hIcon/szTip is left at its zero value (this
// tray never uses balloon notifications or the version/GUID extensions);
// they're still declared so cbSize (computed via unsafe.Sizeof, never a
// hand-picked constant) comes out correct.
type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersionTimeout  uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         guid
	hBalloonIcon     uintptr
}

var (
	mainHwnd        uintptr
	lightIconPath   string
	darkIconPath    string
	trayIconHandle  uintptr
	trayIsLightMode bool
)

func extractIcon(name string) (string, error) {
	data, err := iconFS.ReadFile("icons/" + name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(os.TempDir(), "applivery-soar-"+name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

// isLightTheme reads the taskbar/tray theme (distinct from the "apps" theme
// — SystemUsesLightTheme, not AppsUseLightTheme) from the per-user registry.
// Defaults to light (Windows' historical out-of-box default) if the key or
// value is missing, e.g. on a Windows Server SKU that doesn't expose this
// personalization key at all.
func isLightTheme() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("SystemUsesLightTheme")
	if err != nil {
		return true
	}
	return v != 0
}

func loadThemedIcon(light bool) uintptr {
	path := darkIconPath
	if light {
		path = lightIconPath
	}
	if path == "" {
		return 0
	}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	cx, _, _ := procGetSystemMetrics.Call(uintptr(smCxSmIcon))
	cy, _, _ := procGetSystemMetrics.Call(uintptr(smCySmIcon))
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(pathPtr)), uintptr(imageIcon), cx, cy, uintptr(lrLoadFromFile))
	return h
}

func copyToUTF16Buf(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		u, _ = syscall.UTF16FromString(strings.ReplaceAll(s, "\x00", ""))
	}
	n := len(u)
	if n > len(dst) {
		n = len(dst)
	}
	copy(dst[:n], u[:n])
	if n == len(dst) && n > 0 {
		dst[n-1] = 0
	}
}

func readStatusCache() (*agentstatus.StatusCache, error) {
	data, err := os.ReadFile(agentstatus.CachePath())
	if err != nil {
		return nil, err
	}
	var cache agentstatus.StatusCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

func buildTooltip() string {
	cache, err := readStatusCache()
	if err != nil || cache == nil {
		return "Applivery SOAR Agent — waiting for first report"
	}
	if !cache.Compliance.Available {
		return "Applivery SOAR Agent — compliance status unavailable"
	}
	if cache.Compliance.Compliant {
		return "Applivery SOAR Agent — Compliant"
	}
	return fmt.Sprintf("Applivery SOAR Agent — %d compliance issue(s)", len(cache.Compliance.Violations))
}

func addTrayIcon() {
	trayIsLightMode = isLightTheme()
	trayIconHandle = loadThemedIcon(trayIsLightMode)

	data := notifyIconData{
		cbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		hWnd:             mainHwnd,
		uID:              trayIconID,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayIcon,
		hIcon:            trayIconHandle,
	}
	copyToUTF16Buf(data.szTip[:], buildTooltip())
	procShellNotifyIconW.Call(uintptr(nimAdd), uintptr(unsafe.Pointer(&data)))
}

// refreshTrayIcon re-reads the theme and status cache — called on
// WM_SETTINGCHANGE (theme changed) and on the 60s backstop timer (status
// cache updated by a new report cycle, or WM_SETTINGCHANGE missed for any
// reason). Only swaps the icon handle (and destroys the old one, to avoid
// leaking a GDI icon handle every time the theme flips) when the theme
// actually changed; the tooltip is always refreshed since the underlying
// status.json can change every cycle even with no theme change.
func refreshTrayIcon() {
	light := isLightTheme()
	if light != trayIsLightMode {
		newIcon := loadThemedIcon(light)
		if newIcon != 0 {
			oldIcon := trayIconHandle
			trayIconHandle = newIcon
			trayIsLightMode = light
			if oldIcon != 0 {
				procDestroyIcon.Call(oldIcon)
			}
		}
	}

	data := notifyIconData{
		cbSize: uint32(unsafe.Sizeof(notifyIconData{})),
		hWnd:   mainHwnd,
		uID:    trayIconID,
		uFlags: nifIcon | nifTip,
		hIcon:  trayIconHandle,
	}
	copyToUTF16Buf(data.szTip[:], buildTooltip())
	procShellNotifyIconW.Call(uintptr(nimModify), uintptr(unsafe.Pointer(&data)))
}

func removeTrayIcon() {
	data := notifyIconData{cbSize: uint32(unsafe.Sizeof(notifyIconData{})), hWnd: mainHwnd, uID: trayIconID}
	procShellNotifyIconW.Call(uintptr(nimDelete), uintptr(unsafe.Pointer(&data)))
}

func appendInfo(hMenu uintptr, text string) {
	p, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procAppendMenuW.Call(hMenu, uintptr(mfString|mfGrayed|mfDisabled), uintptr(cmdInfo), uintptr(unsafe.Pointer(p)))
}

func appendSeparator(hMenu uintptr) {
	procAppendMenuW.Call(hMenu, uintptr(mfSeparator), 0, 0)
}

func appendAction(hMenu uintptr, id uintptr, text string) {
	p, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procAppendMenuW.Call(hMenu, uintptr(mfString), id, uintptr(unsafe.Pointer(p)))
}

func formatRelativeTime(rfc3339 string) string {
	if rfc3339 == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func onOff(b bool) string {
	if b {
		return "Enabled"
	}
	return "Disabled"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func dashboardURL(cache *agentstatus.StatusCache) string {
	if cache != nil && cache.BaseURL != "" {
		return cache.BaseURL
	}
	return "https://soar.mi-labs.es"
}

func openURL(u string) {
	if u == "" {
		return
	}
	op, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return
	}
	file, err := syscall.UTF16PtrFromString(u)
	if err != nil {
		return
	}
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(file)), 0, 0, uintptr(swShowNormal))
}

// showContextMenu builds the right-click menu fresh from status.json every
// time — see this file's package doc for why that's deliberate. Everything
// except "Open SOAR Dashboard"/"Exit tray icon" is a disabled (MF_GRAYED |
// MF_DISABLED) informational line: this menu is a status readout, not a
// control surface for the agent itself (stopping/starting the underlying
// service is intentionally not exposed here).
func showContextMenu() {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	cache, cacheErr := readStatusCache()

	appendInfo(hMenu, "Applivery SOAR Agent")
	appendSeparator(hMenu)

	if cacheErr != nil || cache == nil {
		appendInfo(hMenu, "No data reported yet")
		appendInfo(hMenu, "Waiting for the first report cycle…")
	} else {
		if cache.WorkspaceSlug != "" {
			appendInfo(hMenu, fmt.Sprintf("Workspace: %s", cache.WorkspaceSlug))
		}
		reportLine := fmt.Sprintf("Last report: %s", formatRelativeTime(cache.LastReportAt))
		if cache.LastReportOK {
			reportLine += " (OK)"
		} else {
			reportLine += " (failed)"
		}
		appendInfo(hMenu, reportLine)

		if cache.ReportedBitLocker && cache.BitLockerStatus != nil {
			appendInfo(hMenu, fmt.Sprintf("BitLocker: %s", onOff(*cache.BitLockerStatus)))
		}
		if cache.ReportedFirewall && cache.FirewallEnabled != nil {
			appendInfo(hMenu, fmt.Sprintf("Firewall: %s", onOff(*cache.FirewallEnabled)))
		}
		if cache.ReportedApps {
			appendInfo(hMenu, "App inventory: reported")
		}

		appendSeparator(hMenu)

		comp := cache.Compliance
		if !comp.Available {
			appendInfo(hMenu, "Compliance: unavailable")
			if comp.Reason != "" {
				appendInfo(hMenu, truncate(comp.Reason, 60))
			}
		} else {
			if comp.Compliant {
				appendInfo(hMenu, "Compliance: Compliant")
			} else {
				appendInfo(hMenu, fmt.Sprintf("Compliance: %d issue(s) found", len(comp.Violations)))
			}
			if comp.RiskScore != nil && comp.RiskTier != nil {
				appendInfo(hMenu, fmt.Sprintf("Risk score: %d (%s)", *comp.RiskScore, *comp.RiskTier))
			}
			if len(comp.Policies) > 0 {
				appendSeparator(hMenu)
				appendInfo(hMenu, fmt.Sprintf("Policies applied (%d):", len(comp.Policies)))
				violated := make(map[string]bool, len(comp.Violations))
				for _, v := range comp.Violations {
					violated[v.PolicyID] = true
				}
				const maxShown = 6
				shown := 0
				for _, p := range comp.Policies {
					if shown >= maxShown {
						break
					}
					mark := "OK"
					if violated[p.ID] {
						mark = "VIOLATION"
					}
					appendInfo(hMenu, fmt.Sprintf("  %s — %s", truncate(p.Name, 32), mark))
					shown++
				}
				if len(comp.Policies) > shown {
					appendInfo(hMenu, fmt.Sprintf("  …and %d more", len(comp.Policies)-shown))
				}
			}
		}
	}

	appendSeparator(hMenu)
	appendAction(hMenu, cmdOpenDashboard, "Open SOAR Dashboard")
	appendAction(hMenu, cmdExit, "Exit tray icon")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWin.Call(mainHwnd)

	ret, _, _ := procTrackPopupMenuEx.Call(hMenu, uintptr(tpmReturnCmd|tpmRightAlign|tpmBottomAlign), uintptr(pt.x), uintptr(pt.y), mainHwnd, 0)
	// The classic "menu doesn't disappear when you click elsewhere" fix —
	// TrackPopupMenu(Ex)'s own documented workaround.
	procPostMessageW.Call(mainHwnd, 0, 0, 0)

	switch uint32(ret) {
	case cmdOpenDashboard:
		openURL(dashboardURL(cache))
	case cmdExit:
		procDestroyWindow.Call(mainHwnd)
	}
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch uint32(msg) {
	case wmTrayIcon:
		switch uint32(lParam) {
		case wmRButtonUp, wmLButtonUp:
			showContextMenu()
		}
		return 0
	case wmSettingChange:
		refreshTrayIcon()
		return 0
	case wmTimer:
		refreshTrayIcon()
		return 0
	case wmDestroy:
		procKillTimer.Call(mainHwnd, uintptr(refreshTimerID))
		removeTrayIcon()
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return ret
}

func main() {
	agentlog.Setup("tray")
	log.Println("Applivery SOAR Tray starting…")

	var err error
	lightIconPath, err = extractIcon("tray_light.ico")
	if err != nil {
		log.Printf("Could not extract light-theme icon: %v", err)
	}
	darkIconPath, err = extractIcon("tray_dark.ico")
	if err != nil {
		log.Printf("Could not extract dark-theme icon: %v", err)
	}

	hInst, _, _ := procGetModuleHandleW.Call(0)

	className, err := syscall.UTF16PtrFromString("ApplivierySOARTrayWndClass")
	if err != nil {
		log.Fatalf("UTF16PtrFromString(class name) failed: %v", err)
	}

	wc := wndClassExW{
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     hInst,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))

	atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		log.Fatalf("RegisterClassExW failed: %v", callErr)
	}

	winTitle, err := syscall.UTF16PtrFromString("Applivery SOAR Tray")
	if err != nil {
		log.Fatalf("UTF16PtrFromString(window title) failed: %v", err)
	}

	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(winTitle)),
		0,
		0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		log.Fatalf("CreateWindowExW failed: %v", callErr)
	}
	mainHwnd = hwnd

	addTrayIcon()
	procSetTimer.Call(mainHwnd, uintptr(refreshTimerID), uintptr(refreshIntervalMs), 0)

	log.Println("Tray icon active — entering message loop.")

	var m msgT
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	log.Println("Applivery SOAR Tray exiting.")
}
