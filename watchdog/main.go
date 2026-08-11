//go:build windows
// +build windows

// The Applivery SOAR Watchdog service. Its only job is watching the main
// AppliverySOARAgent service and restarting it if it's found stopped — the
// mutual half of the anti-tampering design: AppliverySOARAgent itself runs
// the same check against THIS service (see runWatchdogPartnerCheck in
// watchdog_windows.go, one directory up), so an admin who stops either
// service alone gets it restarted by its partner within one poll interval.
// Stopping BOTH within that same short window (or removing the services
// entirely) still defeats it — this is a deterrent against casual/
// accidental tampering, same tier of protection real-world EDR/MDM agents
// commonly ship, not a kernel-mode guarantee.
//
// Deliberately minimal: no telemetry, no HTTP, no Managed Configuration —
// just svc scaffolding (mirroring the main agent's own main.go) around
// internal/svcwatch's polling loop.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/raulcnadal/applivery-soar-agent-windows/internal/agentlog"
	"github.com/raulcnadal/applivery-soar-agent-windows/internal/svcwatch"
)

const (
	watchdogServiceName = "AppliverySOARWatchdog"
	agentServiceName    = "AppliverySOARAgent"

	pollInterval = 30 * time.Second
	graceDelay   = 60 * time.Second
)

type watchdogService struct{}

func (m *watchdogService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	stopChan := make(chan struct{})
	go svcwatch.Monitor(agentServiceName, pollInterval, graceDelay, stopChan)

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			// No special handling needed here beyond stopping our own
			// monitor loop — a legitimate SCM-driven stop (uninstall,
			// upgrade, system shutdown, or an admin who's about to also
			// stop the main agent) should never trigger this service to
			// fight back. It just goes quiet.
			close(stopChan)
			changes <- svc.Status{State: svc.StopPending}
			return
		}
	}
	return
}

func main() {
	agentlog.Setup("watchdog")

	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Fatalf("Failed to determine if running interactively: %v", err)
	}

	if isInteractive {
		log.Println("Running interactively (Debug Mode)")
		stopChan := make(chan struct{})
		go svcwatch.Monitor(agentServiceName, pollInterval, graceDelay, stopChan)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Interrupted — shutting down.")
		close(stopChan)
		return
	}

	err = svc.Run(watchdogServiceName, &watchdogService{})
	if err != nil {
		log.Fatalf("Failed to start Windows service: %v", err)
	}
}
