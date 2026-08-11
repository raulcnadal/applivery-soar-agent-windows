//go:build windows
// +build windows

package main

import (
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows/svc"
)

// bestEffortConsoleWriter wraps the log file so a broken/absent console
// can never block the write that actually matters. This build is always
// linked with -H windowsgui (deliberately — so no console window flashes up
// when SCM starts this as a service), which means os.Stdout has no real
// console handle attached: writing to it returns an error, both when SCM
// launches the service AND when the exe is run directly from an existing
// terminal (a windowsgui-subsystem process detaches from the launching
// console entirely — this is also why running the exe by hand from
// PowerShell returns instantly with no output instead of blocking). Using
// io.MultiWriter(os.Stdout, file) here was the actual bug: MultiWriter
// aborts on the first writer that errors, so a broken os.Stdout silently
// stopped every single log.Printf across the whole agent from ever reaching
// the file — the log directory and empty file got created by
// setupFileLogging's own os.OpenFile, but not one byte after that ever
// landed on disk, in either interactive or service mode. This type makes
// the file the writer that must succeed and lets stdout fail silently.
type bestEffortConsoleWriter struct {
	file io.Writer
}

func (w bestEffortConsoleWriter) Write(p []byte) (int, error) {
	_, _ = os.Stdout.Write(p) // best-effort only — a real console, if attached, still gets output; a broken one can't block the file write below.
	return w.file.Write(p)
}

// setupFileLogging redirects the standard logger to
// %ProgramData%\Applivery\SOAR\agent.log. This exists because a Windows
// Service has no console at all — log.Println's default os.Stderr
// destination goes nowhere once installed via the MSI and started by SCM,
// so until now there was no way to see this agent's own diagnostics ("did
// it read the registry key?", "did the POST succeed?") without stopping the
// service and re-running the exe interactively. A crude size-based rotation
// (single .old backup) keeps this from growing unbounded over months of
// hourly cycles.
func setupFileLogging() {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	logDir := filepath.Join(programData, "Applivery", "SOAR")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("Could not create log directory %s: %v — logging to console only.", logDir, err)
		return
	}
	logPath := filepath.Join(logDir, "agent.log")

	const maxLogSize = 10 * 1024 * 1024 // 10 MB
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogSize {
		_ = os.Rename(logPath, logPath+".old")
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Could not open log file %s: %v — logging to console only.", logPath, err)
		return
	}
	log.SetOutput(bestEffortConsoleWriter{file: f})
	log.Printf("Logging to %s", logPath)
}

type agentService struct{}

func (m *agentService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	stopChan := make(chan struct{})

	go runAgentLoop(stopChan)

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
	setupFileLogging()

	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Fatalf("Failed to determine if running interactively: %v", err)
	}

	if isInteractive {
		log.Println("Running interactively (Debug Mode)")
		stopChan := make(chan struct{})

		go runAgentLoop(stopChan)

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