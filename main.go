//go:build windows
// +build windows

package main

import (
	"log"
	"os"

	"golang.org/x/sys/windows/svc"
)

type agentService struct{}

// Execute is called by the Windows Service Control Manager
func (m *agentService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	// Load config using your existing registry_windows.go logic
	config := LoadConfig()
	stopChan := make(chan struct{})

	// Start the telemetry execution loop in the background
	go runAgentLoop(config, stopChan)

	// Wait for Windows to tell us to stop
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			close(stopChan)
			changes <- svc.Status{State: svc.StopPending}
			return
		}
	}
	return
}

func main() {
	// Check if running as a real Windows service or just a manual test
	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Fatalf("Failed to determine if we are running interactively: %v", err)
	}

	if isInteractive {
		log.Println("Running interactively (Debug Mode)")
		config := LoadConfig()
		stopChan := make(chan struct{})
		
		// Run directly in the console
		runAgentLoop(config, stopChan)
		os.Exit(0)
	}

	// Run as a background service
	err = svc.Run("AppliverySOARAgent", &agentService{})
	if err != nil {
		log.Fatalf("Service failed: %v", err)
	}
}