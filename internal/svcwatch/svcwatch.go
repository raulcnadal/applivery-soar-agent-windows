//go:build windows
// +build windows

// Package svcwatch implements the mutual-watchdog primitive shared by the
// main Applivery SOAR Agent service and its companion AppliverySOARWatchdog
// service: "is my partner service running, and if not, start it back up."
//
// This exists because a local administrator can stop either service outright
// (`sc stop`, services.msc, Task Manager's Services tab) — Windows Service
// Recovery Actions (the built-in "restart on failure" configured per
// service) only fire on a crash/non-zero exit, never on a clean SCM stop, so
// they don't help against deliberate tampering. Two services that each
// watch the other and restart it on an unexpected stop raises the bar
// considerably: an admin who stops one gets it silently restarted within one
// poll interval by its partner, so defeating protection for real requires
// noticing and stopping BOTH within that same window. This is a deterrent,
// not a kernel-mode guarantee — a determined local admin who stops both
// services (or deletes them, or kills the processes and blocks SCM restart)
// can still ultimately win, same as with every other user-mode agent
// protection. See watchdog/main.go's package doc for the full picture.
package svcwatch

import (
	"log"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// EnsureRunning opens `serviceName` via the Service Control Manager and, if
// it's found in the SERVICE_STOPPED state, starts it. Every other state
// (running, some pending transition, or "doesn't exist / SCM unreachable")
// is left alone and just logged — pending transitions get a chance to
// finish on their own, and a missing/unreachable service is something this
// process can't fix by calling Start on it again.
func EnsureRunning(serviceName string) {
	m, err := mgr.Connect()
	if err != nil {
		log.Printf("[svcwatch] Could not connect to the Service Control Manager: %v", err)
		return
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		log.Printf("[svcwatch] Could not open service %q (not installed, or a permissions issue): %v", serviceName, err)
		return
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		log.Printf("[svcwatch] Could not query status of service %q: %v", serviceName, err)
		return
	}

	if status.State != svc.Stopped {
		return
	}

	log.Printf("[svcwatch] Service %q is stopped — restarting it.", serviceName)
	if err := s.Start(); err != nil {
		log.Printf("[svcwatch] Failed to start service %q: %v", serviceName, err)
		return
	}
	log.Printf("[svcwatch] Service %q restart requested successfully.", serviceName)
}

// Monitor calls EnsureRunning(partnerServiceName) every `interval`, until
// stopChan is closed. `graceDelay` is how long to wait after this process's
// own start before the very first check — avoids a normal system boot
// racing two services that are both still starting up against each other
// (each briefly sees the other as "not running yet" and tries to
// force-start it while SCM is already bringing it up on its own).
func Monitor(partnerServiceName string, interval, graceDelay time.Duration, stopChan <-chan struct{}) {
	MonitorFunc(func() { EnsureRunning(partnerServiceName) }, interval, graceDelay, stopChan)
}

// MonitorFunc is Monitor's generic counterpart: the same start-delay,
// interval, stop-channel polling loop, but driven by an arbitrary check
// function instead of always being "is this SCM service running". Monitor
// itself is now just MonitorFunc with EnsureRunning bound in as the check —
// added so EnsureTrayRunning (tray.go, same package) can reuse this exact
// polling loop even though the tray helper has no SCM state to query.
func MonitorFunc(check func(), interval, graceDelay time.Duration, stopChan <-chan struct{}) {
	select {
	case <-time.After(graceDelay):
	case <-stopChan:
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	check()
	for {
		select {
		case <-ticker.C:
			check()
		case <-stopChan:
			return
		}
	}
}
