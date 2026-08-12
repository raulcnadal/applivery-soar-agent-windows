//go:build windows
// +build windows

// EnsureTrayRunning is EnsureRunning's counterpart for the per-user tray
// helper (tray/main.go) — which, unlike AppliverySOARAgent/AppliverySOARWatchdog,
// is deliberately NOT a Windows service (Session 0 has no desktop, so a
// service can never show a tray icon — see tray/main.go's own doc comment).
// Its only supervision today is agent.wxs's ONLOGON-triggered Scheduled Task
// ("Applivery SOAR Tray"), which — as the README's Troubleshooting section
// already documented before this file existed — only fires once per logon: a
// tray process that's killed, crashes, or is closed mid-session doesn't come
// back on its own until the user logs off and back on. This file closes that
// gap the same way EnsureRunning closes it for the two services: notice the
// process is gone, and bring it back, silently.
package svcwatch

import (
	"log"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

const (
	th32csSnapProcess = 0x00000002
	invalidHandleTray = ^uintptr(0)
)

var (
	modkernel32Tray = syscall.NewLazyDLL("kernel32.dll")

	procCreateToolhelp32Snapshot     = modkernel32Tray.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW              = modkernel32Tray.NewProc("Process32FirstW")
	procProcess32NextW               = modkernel32Tray.NewProc("Process32NextW")
	procCloseHandleTray              = modkernel32Tray.NewProc("CloseHandle")
	procWTSGetActiveConsoleSessionId = modkernel32Tray.NewProc("WTSGetActiveConsoleSessionId")
)

// processEntry32W mirrors PROCESSENTRY32W (tlhelp32.h). Only szExeFile is
// ever read, but every field must still be present and correctly typed/
// ordered for CreateToolhelp32Snapshot's enumeration calls to walk the
// struct correctly — dwSize in particular is validated by Process32FirstW
// against exactly this layout's size.
type processEntry32W struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [260]uint16
}

// isProcessRunning enumerates every process system-wide (CreateToolhelp32Snapshot
// isn't scoped to a session — this watchdog runs in Session 0 as LocalSystem
// and has no session-local process list of its own to check against instead)
// and reports whether any of them is named exeName (case-insensitive match
// against the bare file name, not a full path).
//
// If the snapshot itself can't be taken, this fails safe by reporting "yes,
// it's running" — the alternative (assume "no") risks re-running the
// Scheduled Task on every single poll interval forever whenever this one
// diagnostic call happens to be failing, which is worse than just skipping
// this poll's check.
func isProcessRunning(exeName string) bool {
	snap, _, _ := procCreateToolhelp32Snapshot.Call(uintptr(th32csSnapProcess), 0)
	if snap == 0 || snap == invalidHandleTray {
		log.Printf("[svcwatch] CreateToolhelp32Snapshot failed — skipping this tray process check.")
		return true
	}
	defer procCloseHandleTray.Call(snap)

	target := strings.ToLower(exeName)

	var entry processEntry32W
	entry.dwSize = uint32(unsafe.Sizeof(entry))
	ret, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&entry)))
	for ret != 0 {
		if strings.ToLower(syscall.UTF16ToString(entry.szExeFile[:])) == target {
			return true
		}
		entry = processEntry32W{dwSize: uint32(unsafe.Sizeof(entry))}
		ret, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&entry)))
	}
	return false
}

// hasActiveConsoleSession reports whether anyone is currently logged on to
// the physical console (locked-but-logged-in still counts; only "nobody has
// logged on at all yet", e.g. straight after a boot, returns the sentinel
// 0xFFFFFFFF) — no point re-running the tray's logon task if there's no one
// signed in for it to show a tray icon to. WTSGetActiveConsoleSessionId is,
// despite the WTS prefix, exported directly by kernel32.dll — no separate
// wtsapi32.dll import needed.
func hasActiveConsoleSession() bool {
	id, _, _ := procWTSGetActiveConsoleSessionId.Call()
	return uint32(id) != 0xFFFFFFFF
}

// EnsureTrayRunning checks whether exeName is currently running anywhere on
// the system and, if it isn't (and someone is actually logged on to re-run
// it for), re-triggers taskName via schtasks.exe /Run.
//
// Deliberately reuses the existing Scheduled Task rather than launching
// exeName directly with CreateProcessAsUser: agent.wxs's RegisterTrayTask
// already has the exact right principal/session-targeting configured
// (/RU "Users" /RL LIMITED — standard-user rights, in the interactively
// logged-on user's own session, not this LocalSystem service's Session 0),
// which is precisely what WTSQueryUserToken + CreateProcessAsUser would
// otherwise have to reimplement by hand — including enabling SeTcbPrivilege
// on this process's own token first — in a codebase with no local Windows
// environment available to verify that sequence against. schtasks.exe /Run
// asks the Task Scheduler service to execute the task through its own,
// already-correct launch path (the same one the ONLOGON trigger itself
// uses), so none of that needs reimplementing here.
//
// Known limitation, stated plainly rather than oversold: with multiple
// concurrent interactive sessions (fast user switching, or a console user
// plus an RDP session), a manually-triggered /Run has less precise
// per-session targeting than the ONLOGON trigger firing naturally once per
// session — this is a best-effort recovery for the common single-session
// case, not a guarantee across every multi-session topology.
func EnsureTrayRunning(taskName, exeName string) {
	if !hasActiveConsoleSession() {
		return
	}
	if isProcessRunning(exeName) {
		return
	}
	log.Printf("[svcwatch] %q is not running — re-running Scheduled Task %q.", exeName, taskName)
	out, err := exec.Command("schtasks", "/Run", "/TN", taskName).CombinedOutput()
	if err != nil {
		log.Printf("[svcwatch] Failed to run Scheduled Task %q: %v (%s)", taskName, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("[svcwatch] Scheduled Task %q run requested successfully.", taskName)
}
