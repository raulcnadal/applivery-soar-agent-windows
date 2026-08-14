//go:build windows
// +build windows

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

// Event-driven change detection ("fast lane") — additive to, never a
// replacement for, the normal per-cycle polling in telemetry_windows.go (see
// backend/docs/event-driven-agent-detection-roadmap.md in the main SOAR repo
// for the full design and the reasoning behind that "additive" decision).
//
// Shape of the feature:
//   - Once per report cycle, gatherAndReport() calls syncEventWatches, which
//     polls GET /api/device-data/event-watches?platform=windows (mirroring
//     fetchCustomChecks's exact pattern below) and diffs the result against
//     whichever registry watchers are currently running, starting/stopping/
//     restarting goroutines to match. This is config-driven end to end: an
//     admin adding, editing, or deleting a watch in Settings takes effect on
//     this device's very next cycle, no agent code change or restart needed.
//   - Each registryWatch is a long-lived goroutine, independent of the report
//     ticker, blocked on RegNotifyChangeKeyValue (via a hand-rolled
//     advapi32.dll binding — golang.org/x/sys/windows/registry doesn't wrap
//     this call) waiting for the OS to signal a change under the configured
//     key.
//   - On signal, the watcher re-arms itself (RegNotifyChangeKeyValue delivers
//     exactly one notification per call) and hands off to the shared
//     debouncer: "event storm" protection per the spec — a burst of registry
//     writes (e.g. a single app install touching Uninstall + a dozen
//     sub-keys) resets a per-watch timer instead of firing repeatedly, and
//     only once the key goes quiet for DebounceMs does the agent actually
//     notify SOAR. Exactly one POST /api/device-data/event-notify per burst.
//   - notifyEventFired reloads Config and the serial number fresh at fire
//     time (not captured at watcher-start time) — the same "always reload,
//     never trust a value that might be stale" philosophy gatherAndReport
//     already uses for BaseURL/WorkspaceSlug/ReportSecret. A registry watcher
//     can live for a long time between config changes, so this matters more
//     here than it does for the once-a-cycle custom checks poll.

// EventWatchDef mirrors eventWatches.service.ts's listEnabledWatchesForAgent
// DTO exactly ({key, watchType, params, debounceMs} — no id/name/description/
// action: those are admin/routing concerns, not agent concerns).
type EventWatchDef struct {
	Key        string                 `json:"key"`
	WatchType  string                 `json:"watchType"`
	Params     map[string]interface{} `json:"params"`
	DebounceMs int                    `json:"debounceMs"`
}

// fetchEventWatches mirrors fetchCustomChecks (customchecks_windows.go)
// field-for-field: same client timeout, same header pair, same
// "log and return nil/0 on any failure, let the next cycle retry" behavior.
// Second return value is the Phase 4 remoteIntervalSec override (0 = none —
// see backend's GET /api/device-data/event-watches doc comment for why it
// rides along in this same response instead of a separate endpoint).
func fetchEventWatches(baseURL *url.URL, config Config) ([]EventWatchDef, int) {
	watchesURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/event-watches", RawQuery: "platform=windows"}).String()

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", watchesURL, nil)
	if err != nil {
		log.Printf("Error building event-watches poll request: %v", err)
		return nil, 0
	}
	req.Header.Set("X-Workspace-Slug", config.WorkspaceSlug)
	req.Header.Set("X-Device-Report-Secret", config.ReportSecret)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error polling event watches: %v", err)
		return nil, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Event watches poll returned HTTP %d — skipping this cycle's sync", resp.StatusCode)
		return nil, 0
	}

	var body struct {
		Items             []EventWatchDef `json:"items"`
		RemoteIntervalSec *int            `json:"remoteIntervalSec"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("Error decoding event watches response: %v", err)
		return nil, 0
	}
	remoteIntervalSec := 0
	if body.RemoteIntervalSec != nil {
		remoteIntervalSec = *body.RemoteIntervalSec
	}
	return body.Items, remoteIntervalSec
}

// EventNotifyPayload mirrors deviceData.schemas.ts's eventNotifyPayloadSchema.
type EventNotifyPayload struct {
	Platform        string `json:"platform"`
	SerialNumber    string `json:"serialNumber"`
	WatchKey        string `json:"watchKey"`
	ClientTimestamp string `json:"clientTimestamp,omitempty"`
	// Phase 4 metrics — how many times bump() was called on this key before
	// the debounce window went quiet and fired. Always >=1 when present
	// (bump is called at least once to arm the timer in the first place).
	RawEventCount int `json:"rawEventCount,omitempty"`
}

// --- debounce ---------------------------------------------------------

// debouncer implements the spec's "event storm" handling verbatim: bump
// resets any pending timer for that key; only once bump hasn't been called
// again within delay does fire actually run. One timer per watch key, so an
// unrelated watch's activity never interferes with this one's debounce.
// counts tracks how many times bump has been called for a key since the
// last fire — handed to fire() as rawEventCount, the Phase 4
// "debounce-collapse ratio" metric's raw material.
type debouncer struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
	counts map[string]int
}

func newDebouncer() *debouncer {
	return &debouncer{timers: make(map[string]*time.Timer), counts: make(map[string]int)}
}

func (d *debouncer) bump(key string, delay time.Duration, fire func(rawEventCount int)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[key]; ok {
		t.Stop()
	}
	d.counts[key]++
	d.timers[key] = time.AfterFunc(delay, func() {
		d.mu.Lock()
		count := d.counts[key]
		delete(d.timers, key)
		delete(d.counts, key)
		d.mu.Unlock()
		fire(count)
	})
}

// stop cancels a key's pending timer, if any — used when a watch is removed
// so a stale burst from just before deletion can't fire after the fact.
func (d *debouncer) stop(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[key]; ok {
		t.Stop()
		delete(d.timers, key)
	}
	delete(d.counts, key)
}

// --- watcher manager ----------------------------------------------------

type registryWatch struct {
	key          string
	hive         registry.Key
	path         string
	watchSubtree bool
	stopChan     chan struct{}
}

var (
	watchManagerMu   sync.Mutex
	activeWatchers   = make(map[string]*registryWatch) // keyed by EventWatchDef.Key
	watcherDebouncer = newDebouncer()
)

// syncEventWatches is called once per report cycle from gatherAndReport
// (telemetry_windows.go), right alongside the existing custom-checks poll.
// It polls the config exactly once, then hands the full list off to each
// watchType's own sync function — syncRegistryWatches here, syncEtwWatches
// in etw_windows.go — so adding a third watchType later only means adding a
// third dispatch line, not touching the polling itself. Returns the polled
// remoteIntervalSec (0 = no override) so gatherAndReport can fold it into
// this cycle's effective report interval — see telemetry_windows.go's
// maybeResetTicker.
func syncEventWatches(baseURL *url.URL, config Config) int {
	watches, remoteIntervalSec := fetchEventWatches(baseURL, config)
	syncRegistryWatches(watches)
	syncEtwWatches(watches)
	return remoteIntervalSec
}

// syncRegistryWatches diffs the freshly polled config against
// activeWatchers: starts watchers for new keys, restarts ones whose
// registry target changed, and stops ones that were deleted/disabled/
// changed to a different watchType.
func syncRegistryWatches(watches []EventWatchDef) {
	watchManagerMu.Lock()
	defer watchManagerMu.Unlock()

	seen := make(map[string]bool, len(watches))
	for _, w := range watches {
		if w.WatchType != "registryKey" {
			// Owned by a different sync function (e.g. "etwProvider" —
			// syncEtwWatches in etw_windows.go). Skip silently here.
			continue
		}
		seen[w.Key] = true

		hive, path, watchSubtree, err := parseRegistryKeyParams(w.Params)
		if err != nil {
			log.Printf("Event watch %q: %v — not starting.", w.Key, err)
			continue
		}

		if existing, running := activeWatchers[w.Key]; running {
			if existing.hive == hive && existing.path == path && existing.watchSubtree == watchSubtree {
				continue // unchanged, leave the running watcher alone
			}
			stopRegistryWatcherLocked(w.Key)
		}

		debounceMs := w.DebounceMs
		if debounceMs <= 0 {
			debounceMs = 5000
		}
		watchKey := w.Key
		nw := startRegistryWatcher(watchKey, hive, path, watchSubtree, func() {
			watcherDebouncer.bump(watchKey, time.Duration(debounceMs)*time.Millisecond, func(rawEventCount int) {
				notifyEventFired(watchKey, rawEventCount)
			})
		})
		if nw != nil {
			activeWatchers[watchKey] = nw
		}
	}

	for key := range activeWatchers {
		if !seen[key] {
			stopRegistryWatcherLocked(key)
		}
	}
}

// stopRegistryWatcherLocked must be called with watchManagerMu held.
func stopRegistryWatcherLocked(key string) {
	if w, ok := activeWatchers[key]; ok {
		close(w.stopChan)
		delete(activeWatchers, key)
	}
	watcherDebouncer.stop(key)
}

func parseRegistryKeyParams(params map[string]interface{}) (registry.Key, string, bool, error) {
	hiveStr, _ := params["hive"].(string)
	var hive registry.Key
	switch hiveStr {
	case "HKLM":
		hive = registry.LOCAL_MACHINE
	case "HKCU":
		hive = registry.CURRENT_USER
	default:
		return 0, "", false, fmt.Errorf("unsupported or missing hive %q (must be HKLM or HKCU)", hiveStr)
	}
	path, _ := params["path"].(string)
	if path == "" {
		return 0, "", false, fmt.Errorf("missing registry path")
	}
	watchSubtree, _ := params["watchSubtree"].(bool)
	return hive, path, watchSubtree, nil
}

// startRegistryWatcher launches the long-running goroutine for a single
// watch. It does not block the caller (syncEventWatches, itself called once
// per report cycle) — the goroutine outlives that call and keeps running
// until its stopChan is closed.
func startRegistryWatcher(key string, hive registry.Key, path string, watchSubtree bool, onFire func()) *registryWatch {
	stopChan := make(chan struct{})
	w := &registryWatch{key: key, hive: hive, path: path, watchSubtree: watchSubtree, stopChan: stopChan}

	go func() {
		const reopenBackoff = 60 * time.Second

		for {
			select {
			case <-stopChan:
				return
			default:
			}

			k, err := registry.OpenKey(hive, path, registry.NOTIFY)
			if err != nil {
				log.Printf("Event watch %q: could not open registry key %q — retrying in %s: %v", key, path, reopenBackoff, err)
				select {
				case <-time.After(reopenBackoff):
					continue
				case <-stopChan:
					return
				}
			}

			ev, evErr := createEvent()
			if evErr != nil {
				k.Close()
				log.Printf("Event watch %q: could not create wait event — giving up on this watcher: %v", key, evErr)
				return
			}

			const filter = regNotifyChangeName | regNotifyChangeAttributes | regNotifyChangeLastSet | regNotifyThreadAgnostic
			if notifyErr := regNotifyChangeKeyValue(syscall.Handle(k), watchSubtree, filter, ev, true); notifyErr != nil {
				closeHandle(ev)
				k.Close()
				log.Printf("Event watch %q: RegNotifyChangeKeyValue failed — giving up on this watcher: %v", key, notifyErr)
				return
			}

			result := waitOnHandleOrStop(ev, stopChan)
			closeHandle(ev)
			k.Close()

			if result == waitStopped {
				return
			}
			// waitFired: the key (or, with watchSubtree, a descendant)
			// changed. Hand off to the debouncer and loop around to re-open
			// + re-arm — RegNotifyChangeKeyValue only ever delivers one
			// notification per call.
			onFire()
		}
	}()

	return w
}

// notifyEventFired reloads Config and the serial number fresh (see this
// file's top-of-file doc comment for why) and POSTs to
// /api/device-data/event-notify, reusing sendWebhook (telemetry_windows.go)
// for the same retry/backoff/header behavior every other agent->SOAR call
// gets. rawEventCount (Phase 4 metrics) is however many times this watch's
// debounce timer was bumped before it went quiet — passed straight through
// from the debouncer, not recomputed here.
func notifyEventFired(watchKey string, rawEventCount int) {
	config := LoadConfig()
	if !config.IsConfigured() {
		log.Printf("Event watch %q fired but this agent has no WorkspaceSlug/ReportSecret configured — skipping notify.", watchKey)
		return
	}
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		log.Printf("Event watch %q fired but BaseURL is invalid — skipping notify: %v", watchKey, err)
		return
	}
	serialNumber := GetSerialNumber()
	if !isUsableSerial(serialNumber) {
		log.Printf("Event watch %q fired but serial number is empty/placeholder — skipping notify.", watchKey)
		return
	}

	notifyURL := baseURL.ResolveReference(&url.URL{Path: "/api/device-data/event-notify"}).String()
	payload := EventNotifyPayload{
		Platform:        "windows",
		SerialNumber:    serialNumber,
		WatchKey:        watchKey,
		ClientTimestamp: time.Now().UTC().Format(time.RFC3339),
		RawEventCount:   rawEventCount,
	}
	sendWebhook(notifyURL, config, payload)
}

// --- manual Win32 bindings ----------------------------------------------
//
// golang.org/x/sys/windows/registry gives us OpenKey/Close/GetStringValue
// etc., but deliberately doesn't wrap RegNotifyChangeKeyValue (it's a
// blocking/async primitive, not a simple value accessor) — so this section
// hand-rolls the same kind of syscall.NewLazyDLL/.NewProc binding the
// roadmap doc called for, kept self-contained to this file.

var (
	modAdvapi32 = syscall.NewLazyDLL("advapi32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegNotifyChangeKeyValue = modAdvapi32.NewProc("RegNotifyChangeKeyValue")
	procCreateEventW            = modKernel32.NewProc("CreateEventW")
	procWaitForSingleObject     = modKernel32.NewProc("WaitForSingleObject")
	procCloseHandle             = modKernel32.NewProc("CloseHandle")
)

// REG_NOTIFY_* filter flags (winnt.h) — Name/Attributes/LastSet cover the
// cases that matter for "did an app get installed/uninstalled or a value
// change under this key": a new sub-key appearing or disappearing, and a
// value being written. REG_NOTIFY_THREAD_AGNOSTIC (Windows 8+) is the flag
// that lets this fire without pinning the notification to the OS thread
// that issued it — required because Go's goroutine scheduler doesn't give
// us a stable OS thread to pin to without locking it (runtime.LockOSThread),
// which this deliberately avoids needing.
const (
	regNotifyChangeName       = 0x00000001
	regNotifyChangeAttributes = 0x00000002
	regNotifyChangeLastSet    = 0x00000004
	regNotifyThreadAgnostic   = 0x10000000

	waitObject0  = 0x00000000
	waitTimeout  = 0x00000102
	waitFailed   = 0xFFFFFFFF
)

const (
	waitFired   = 0
	waitStopped = 1
)

// createEvent makes an auto-reset, initially-non-signaled, unnamed event —
// exactly what a one-shot "wait for this one notification" loop needs; each
// registry watcher iteration creates and discards its own.
func createEvent() (syscall.Handle, error) {
	r, _, err := procCreateEventW.Call(0, 0, 0, 0)
	if r == 0 {
		return 0, err
	}
	return syscall.Handle(r), nil
}

func closeHandle(h syscall.Handle) {
	procCloseHandle.Call(uintptr(h))
}

// regNotifyChangeKeyValue's return value IS the Win32 status code (0 =
// ERROR_SUCCESS) — unlike most Win32 calls, it does not rely on
// GetLastError(), so the syscall package's usual r2/err convention doesn't
// apply here; r1 is checked directly.
func regNotifyChangeKeyValue(key syscall.Handle, watchSubtree bool, filter uint32, event syscall.Handle, async bool) error {
	var watchSubtreeArg, asyncArg uintptr
	if watchSubtree {
		watchSubtreeArg = 1
	}
	if async {
		asyncArg = 1
	}
	r1, _, _ := procRegNotifyChangeKeyValue.Call(
		uintptr(key),
		watchSubtreeArg,
		uintptr(filter),
		uintptr(event),
		asyncArg,
	)
	if r1 != 0 {
		return syscall.Errno(r1)
	}
	return nil
}

// waitOnHandleOrStop polls WaitForSingleObject with a short timeout instead
// of blocking indefinitely, specifically so it can also notice stopChan
// being closed — Win32 has no primitive that waits on both a HANDLE and a Go
// channel at once, and a couple of seconds of shutdown latency is a
// non-issue for a feature whose own debounce window is measured in seconds.
func waitOnHandleOrStop(ev syscall.Handle, stopChan <-chan struct{}) int {
	const pollTimeoutMs = 2000
	for {
		select {
		case <-stopChan:
			return waitStopped
		default:
		}
		r, _, err := procWaitForSingleObject.Call(uintptr(ev), uintptr(pollTimeoutMs))
		result := uint32(r)
		if result == waitFailed {
			log.Printf("WaitForSingleObject failed: %v", err)
			return waitStopped
		}
		if result == waitObject0 {
			return waitFired
		}
		// waitTimeout: loop around, re-check stopChan.
	}
}
