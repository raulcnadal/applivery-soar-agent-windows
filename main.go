//go:build windows
// +build windows

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows/svc"
)

type agentService struct{}

func (m *agentService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	config := LoadConfig()
	stopChan := make(chan struct{})

	go runAgentLoop(config, stopChan)

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
	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Fatalf("Failed to determine if running interactively: %v", err)
	}

	if isInteractive {
		log.Println("Running interactively (Debug Mode)")
		config := LoadConfig()
		stopChan := make(chan struct{})
		
		go runAgentLoop(config, stopChan)

		// Block main thread until interrupted. An earlier version of this
		// created sigChan but never registered it with signal.Notify, so
		// Ctrl+C in debug mode never actually reached it — the process
		// could only be killed forcibly.
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Interrupted — shutting down.")
		close(stopChan)
		return
	}

	err = svc.Run("AppliverySOARAgent", &agentService{})
	if err != nil {
		log.Fatalf("Failed to start Windows service: %v", err)
	}
}