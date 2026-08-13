//go:build windows
// +build windows

// The Applivery SOAR Tray helper — a small, unprivileged, per-user-session
// process that shows a persistent notification-area icon so a user always
// has a visible, informational signal that device management/compliance
// reporting is active, plus a click-to-open status card summarizing what's
// being reported and this device's current Compliance Policy status. It is
// deliberately NOT a Windows service: services run in Session 0, which has
// had no desktop/UI access since Windows Vista's session isolation, so a
// service can never show a tray icon in an interactive user's session
// directly. Instead the installer registers a Scheduled Task (agent.wxs)
// that starts this exe once any user logs on.
//
// This process has no registry access to the Managed Configuration secret
// and no HTTP client of its own — everything the card shows comes from
// %ProgramData%\Applivery\SOAR\status.json, written by the main
// AppliverySOARAgent service after every report cycle (status_windows.go /
// internal/agentstatus in the repo root) — that package's doc comment has
// the full rationale for the split. Re-read fresh every time the card is
// opened (and on a 60s timer for the tooltip/icon/notifications), so it's
// never more than one report cycle stale. The card's "Force report"/"Force
// evaluate compliance" buttons are the one exception to "read-only": they
// drop an empty marker file (agentstatus.WriteTrigger) the main service
// polls for, rather than this process calling the backend itself — see
// triggerForceReport/triggerForceEvaluate below.
//
// This agent runs on end-user devices, not admin machines — there is
// deliberately no "open dashboard" link (the dashboard is admin-only) and
// no way to exit/stop the tray or agent from here (see agent.wxs's
// tamper-resistance design in the root README).
//
// Built entirely on raw syscalls against user32.dll/gdi32.dll/shell32.dll
// via the standard library's syscall package — no GUI toolkit or WebView2
// dependency, matching this repo's existing style (wmi_windows.go,
// registry_windows.go) of hand-rolled Win32 calls, and something this
// repo's offline build can actually verify (a new dependency's go.sum
// entries need network access this sandbox doesn't reliably have).
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"github.com/raulcnadal/applivery-soar-agent-windows/internal/agentlog"
	"github.com/raulcnadal/applivery-soar-agent-windows/internal/agentstatus"
)

//go:embed icons/tray_light.ico icons/tray_dark.ico icons/banner_light.bmp icons/banner_dark.bmp
var iconFS embed.FS

const (
	wmDestroy       = 0x0002
	wmSettingChange = 0x001A
	wmTimer         = 0x0113
	wmLButtonUp     = 0x0202
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	wmTrayIcon      = wmApp + 1
	wmShowCard      = wmApp + 2

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo    = 0x00000001
	niifWarning = 0x00000002

	imageIcon      = 1
	lrLoadFromFile = 0x00000010

	smCxSmIcon = 49
	smCySmIcon = 50

	trayIconID        = 1
	refreshTimerID    = 1
	refreshIntervalMs = 60_000
)

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")
	moduser32   = syscall.NewLazyDLL("user32.dll")
	modshell32  = syscall.NewLazyDLL("shell32.dll")

	procGetModuleHandleW              = modkernel32.NewProc("GetModuleHandleW")
	procSetProcessDpiAwarenessContext = moduser32.NewProc("SetProcessDpiAwarenessContext")

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
	procSetForegroundWin = moduser32.NewProc("SetForegroundWindow")
	procGetSystemMetrics = moduser32.NewProc("GetSystemMetrics")
	procSetTimer         = moduser32.NewProc("SetTimer")
	procKillTimer        = moduser32.NewProc("KillTimer")

	procShellNotifyIconW = modshell32.NewProc("Shell_NotifyIconW")
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

// winRect mirrors the Win32 RECT struct — shared by the tray window's own
// (minimal) needs and card.go's much heavier use for layout/hit-testing.
type winRect struct {
	left, top, right, bottom int32
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
// targets. szInfo/szInfoTitle/dwInfoFlags (NIF_INFO) drive the balloon/toast
// notifications fired on a compliance-violation/recovery transition — see
// checkComplianceTransition below.
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
	lightBannerPath string
	darkBannerPath  string
	trayIconHandle  uintptr
	trayIsLightMode bool

	// lastViolationCount tracks the previous poll's violation count so
	// checkComplianceTransition can fire a balloon only on an actual
	// 0->N or N->0 transition, never on every single poll. -1 means "not
	// observed yet" (this process just started) — deliberately not firing
	// a notification for whatever state the device happens to already be
	// in at tray startup, only for changes witnessed while running.
	lastViolationCount = -1
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

// isLightTheme reads the taskbar/tray theme (SystemUsesLightTheme) from the
// per-user registry, falling back to the "apps" theme (AppsUseLightTheme)
// if that specific value isn't present — some Windows editions/versions
// only expose one of the two. Defaults to light (Windows' historical
// out-of-box default) only if neither value is readable at all, e.g. on a
// Windows Server SKU that doesn't expose this personalization key.
func isLightTheme() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("SystemUsesLightTheme"); err == nil {
		return v != 0
	}
	if v, _, err := k.GetIntegerValue("AppsUseLightTheme"); err == nil {
		return v != 0
	}
	return true
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

// showBalloon fires a classic Shell_NotifyIcon balloon (NIF_INFO) — on
// modern Windows this is routed through the Action Center like a toast,
// with zero extra APIs beyond what's already used for the icon/tooltip
// (NOTIFYICONDATAW already declares these fields). Deliberately not the
// WinRT ToastNotificationManager route (COM/WinRT interop this repo has no
// need to take on for a two-message-type notification).
func showBalloon(title, msg string, infoFlags uint32) {
	data := notifyIconData{
		cbSize:      uint32(unsafe.Sizeof(notifyIconData{})),
		hWnd:        mainHwnd,
		uID:         trayIconID,
		uFlags:      nifInfo,
		dwInfoFlags: infoFlags,
	}
	copyToUTF16Buf(data.szInfo[:], msg)
	copyToUTF16Buf(data.szInfoTitle[:], title)
	procShellNotifyIconW.Call(uintptr(nimModify), uintptr(unsafe.Pointer(&data)))
}

// triggerForceReport/triggerForceEvaluate back the status card's "Force
// report"/"Force evaluate compliance" buttons (card.go's cardWndProc). This
// process still has no HTTP client or Managed Configuration secret of its
// own — see this file's top doc comment — so rather than calling the
// backend directly, it drops an empty marker file the already-authenticated
// main service polls for every couple of seconds and acts on
// (agentstatus.WriteTrigger's doc comment has the full design). The balloon
// confirms the request was queued, not that it necessarily succeeded — the
// service's own agent.log has the actual outcome if something goes wrong
// (no Automation Credential configured, a network error, etc.).
func triggerForceReport() {
	if err := agentstatus.WriteTrigger(agentstatus.TriggerReportPath()); err != nil {
		log.Printf("Could not write force-report trigger: %v", err)
		return
	}
	showBalloon("Applivery SOAR", "Reporting to Applivery SOAR now…", niifInfo)
}

func triggerForceEvaluate() {
	if err := agentstatus.WriteTrigger(agentstatus.TriggerEvaluatePath()); err != nil {
		log.Printf("Could not write force-evaluate trigger: %v", err)
		return
	}
	showBalloon("Applivery SOAR", "Evaluating compliance now…", niifInfo)
}

func pluralPolicy(n int) string {
	if n == 1 {
		return "policy"
	}
	return "policies"
}

// checkComplianceTransition compares this poll's violation count against
// the previous one and fires a balloon on a real 0<->N transition — never
// on every poll (which would make status.json's normal per-cycle rewrite
// spam a notification even when nothing about compliance actually changed).
func checkComplianceTransition(cache *agentstatus.StatusCache) {
	if cache == nil || !cache.Compliance.Available {
		return
	}
	count := len(cache.Compliance.Violations)
	if lastViolationCount == -1 {
		lastViolationCount = count
		return
	}
	if count > 0 && lastViolationCount == 0 {
		showBalloon("Compliance issue detected", fmt.Sprintf("%d %s now failing on this device.", count, pluralPolicy(count)), niifWarning)
	} else if count == 0 && lastViolationCount > 0 {
		showBalloon("Compliance restored", "This device is compliant with all applicable policies again.", niifInfo)
	}
	lastViolationCount = count
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

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch uint32(msg) {
	case wmTrayIcon:
		// Explorer delivers the notification-area callback message
		// synchronously (Shell_NotifyIcon's uCallbackMessage, dispatched via
		// SendMessage from inside explorer.exe's own thread) — doing the
		// card's real work (CreateWindowExW, SetForegroundWindow, GDI) right
		// here would run all of that nested inside a cross-process call
		// explorer.exe is blocked waiting on. Post ourselves a message
		// instead and do the actual work on the next, fully independent
		// iteration of this thread's own message loop.
		switch uint32(lParam) {
		case wmRButtonUp, wmLButtonUp:
			procPostMessageW.Call(hwnd, uintptr(wmShowCard), 0, 0)
		}
		return 0
	case wmShowCard:
		showCard()
		return 0
	case wmSettingChange:
		refreshTrayIcon()
		return 0
	case wmTimer:
		refreshTrayIcon()
		if cache, err := readStatusCache(); err == nil {
			checkComplianceTransition(cache)
		}
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
	// Win32 windows, message queues, and window procedures are all bound to
	// the specific OS thread that created them. Go's scheduler is otherwise
	// free to move this goroutine to a different OS thread at any
	// preemption point (async preemption has been on by default since Go
	// 1.14) -- if that happens after we've created windows and started
	// pumping messages on this thread, GetMessageW ends up polling a
	// different, empty queue on the new thread while real input piles up
	// unread on the old one: the process goes fully unresponsive with no
	// error, exactly matching Windows Error Reporting's AppHangB1 and the
	// "wait cursor forever" symptom reported against this tray. Locking the
	// thread here, before any window is created, is the standard, required
	// fix for hand-rolled Win32 GUI code in Go.
	runtime.LockOSThread()

	agentlog.Setup("tray")
	log.Println("Applivery SOAR Tray starting…")

	// Best-effort: makes GetSystemMetrics/window sizing report true physical
	// pixels instead of being silently bitmap-scaled by Windows' DPI
	// virtualization shim for unmanifested processes — the likely root
	// cause of a blurry tray icon/card on a scaled display. Available since
	// Windows 10 1703; a failure here (older Windows) just means this
	// process falls back to system-DPI-aware behavior, not a crash.
	var dpiAwarenessContextPerMonitorAwareV2Src int64 = -4
	dpiAwarenessContextPerMonitorAwareV2 := uintptr(dpiAwarenessContextPerMonitorAwareV2Src)
	procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2)

	var err error
	lightIconPath, err = extractIcon("tray_light.ico")
	if err != nil {
		log.Printf("Could not extract light-theme icon: %v", err)
	}
	darkIconPath, err = extractIcon("tray_dark.ico")
	if err != nil {
		log.Printf("Could not extract dark-theme icon: %v", err)
	}
	lightBannerPath, err = extractIcon("banner_light.bmp")
	if err != nil {
		log.Printf("Could not extract light-theme header banner: %v", err)
	}
	darkBannerPath, err = extractIcon("banner_dark.bmp")
	if err != nil {
		log.Printf("Could not extract dark-theme header banner: %v", err)
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
