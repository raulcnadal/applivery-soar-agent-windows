//go:build windows
// +build windows

// Package agentlog provides the file-logging setup and shared ProgramData
// path used by the watchdog and tray components — factored out once there
// was a second and third process needing "redirect log.Println to a
// rotating file under ProgramData, because this process has no real
// console" instead of copy-pasting main.go's original setupFileLogging a
// second and third time. The main agent (main.go) keeps its own
// long-standing copy of this logic untouched rather than being refactored
// onto this package, to avoid any risk of regressing its already-verified
// logging behavior.
package agentlog

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// bestEffortConsoleWriter wraps the log file so a broken/absent console can
// never block the write that actually matters. Every process in this repo
// is built with -H windowsgui (no console window, whether launched by SCM
// or run directly), so os.Stdout has no real console handle attached in
// practice — see main.go's original doc comment on this exact type for the
// full story of why io.MultiWriter(os.Stdout, file) is the wrong tool here.
type bestEffortConsoleWriter struct {
	file io.Writer
}

func (w bestEffortConsoleWriter) Write(p []byte) (int, error) {
	_, _ = os.Stdout.Write(p) // best-effort only — a broken console can't block the file write below.
	return w.file.Write(p)
}

// Dir returns %ProgramData%\Applivery\SOAR — the shared directory every
// component in this repo logs to and, as of the status cache feature, the
// main agent writes status.json into for the tray app to read.
func Dir() string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "Applivery", "SOAR")
}

// Setup redirects the standard logger to Dir()\<component>.log, with the
// same crude size-based single-backup rotation the main agent has used
// since it first gained file logging.
func Setup(component string) {
	logDir := Dir()
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("Could not create log directory %s: %v — logging to console only.", logDir, err)
		return
	}
	logPath := filepath.Join(logDir, component+".log")

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
