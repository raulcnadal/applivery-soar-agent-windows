//go:build windows
// +build windows

package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xrawsec/golang-etw/etw"
)

// ETW watcher ("etwProvider" watchType) — the second event-driven detection
// mechanism, alongside eventwatch_windows.go's registry watcher (see that
// file's top-of-file doc comment for the overall "additive fast lane, never
// a replacement for polling" design; this file only adds a second mechanism
// under the same debounce/notify plumbing already built there).
//
// github.com/0xrawsec/golang-etw (pure Go, no CGO — see its README) does the
// actual OpenTrace/ProcessTrace/parsing work: for each active etwProvider
// watch, this file starts one real-time ETW session + consumer scoped to
// exactly that watch's provider (and, if configured, a kernel-level EventID
// filter — see buildProviderSpec below), and treats every event that makes
// it through that filter as a "something happened" signal for the shared
// debouncer, same as a registry-key watcher's RegNotifyChangeKeyValue fire.
// One session per active watch (not one shared session multiplexing every
// provider) keeps start/stop/restart-on-config-change trivial to reason
// about, at the cost of one extra session per distinct provider watched —
// an acceptable trade given this feature is scoped to a handful of
// admin-curated watches, not "one per possible ETW provider on the system".
//
// First-class use case (matching the roadmap's own Phase 3 example):
// watching Microsoft-Windows-Kernel-Process for process start/stop
// (EventID 1 = start, EventID 2 = stop) as a config-driven watch, e.g.
// params: {"provider": "Microsoft-Windows-Kernel-Process", "eventIds": [1, 2]}.
// Nothing here hardcodes that provider, though — any provider name or GUID
// registered on the device works the same way.

type etwWatch struct {
	key      string
	spec     string // the exact provider spec string this watch was started with — used to detect "did params actually change" without re-parsing on every poll
	session  *etw.RealTimeSession
	consumer *etw.Consumer
	cancel   context.CancelFunc
}

var (
	etwManagerMu     sync.Mutex
	activeEtwWatches = make(map[string]*etwWatch) // keyed by EventWatchDef.Key
)

// syncEtwWatches mirrors syncRegistryWatches (eventwatch_windows.go)
// field-for-field: diff polled config against activeEtwWatches, start/
// restart/stop to match. Called once per report cycle from
// syncEventWatches with the same watch list syncRegistryWatches sees —
// each function ignores watchTypes it doesn't own.
func syncEtwWatches(watches []EventWatchDef) {
	etwManagerMu.Lock()
	defer etwManagerMu.Unlock()

	seen := make(map[string]bool, len(watches))
	for _, w := range watches {
		if w.WatchType != "etwProvider" {
			continue
		}
		seen[w.Key] = true

		spec, err := buildProviderSpec(w.Params)
		if err != nil {
			log.Printf("Event watch %q: %v — not starting.", w.Key, err)
			continue
		}

		if existing, running := activeEtwWatches[w.Key]; running {
			if existing.spec == spec {
				continue // unchanged, leave the running session alone
			}
			stopEtwWatchLocked(w.Key)
		}

		debounceMs := w.DebounceMs
		if debounceMs <= 0 {
			debounceMs = 5000
		}
		watchKey := w.Key
		nw := startEtwWatcher(watchKey, spec, func() {
			// Reuses the exact same debouncer instance registryKey watches
			// share (eventwatch_windows.go) — a watch key is unique across
			// watchTypes (backend enforces this via the
			// (workspaceSlug, platform, key) unique constraint), so there's
			// no risk of an ETW watch and a registry watch colliding on the
			// same debounce timer.
			watcherDebouncer.bump(watchKey, time.Duration(debounceMs)*time.Millisecond, func() {
				notifyEventFired(watchKey)
			})
		})
		if nw != nil {
			activeEtwWatches[watchKey] = nw
		}
	}

	for key := range activeEtwWatches {
		if !seen[key] {
			stopEtwWatchLocked(key)
		}
	}
}

// stopEtwWatchLocked must be called with etwManagerMu held.
func stopEtwWatchLocked(key string) {
	if w, ok := activeEtwWatches[key]; ok {
		w.cancel()
		w.consumer.Stop()
		w.session.Stop()
		delete(activeEtwWatches, key)
	}
	watcherDebouncer.stop(key)
}

// buildProviderSpec turns this watch's admin-authored params into the
// colon-delimited string golang-etw's ParseProvider expects:
// "Name:EnableLevel:EventIDs:MatchAnyKeyword:MatchAllKeyword" (see
// provider.go's ParseProvider doc comment upstream). EventIDs, when given,
// become a kernel-level filter on the ETW session itself (Provider.Filter /
// BuildFilterDesc) — cheaper and more precise than filtering in this
// agent's own event loop, which is why eventIds is a first-class params
// field rather than something this agent post-filters after the fact.
//
// Supported params (see eventWatches.schemas.ts's validateWatchParams for
// the backend-side mirror of this contract):
//   - provider (required): provider name (e.g. "Microsoft-Windows-Kernel-Process")
//     or GUID, exactly as ETW itself would resolve it.
//   - eventIds (optional): array of numbers — if omitted/empty, every event
//     from the provider (at the given level) matches.
//   - level (optional): 0-255, defaults to the library's own DefaultProvider
//     level (0xff / verbose) when omitted.
func buildProviderSpec(params map[string]interface{}) (string, error) {
	name, _ := params["provider"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("missing provider name")
	}

	level := ""
	if raw, ok := params["level"]; ok {
		switch v := raw.(type) {
		case float64:
			level = strconv.Itoa(int(v))
		case string:
			level = strings.TrimSpace(v)
		}
	}

	var eventIDs string
	if raw, ok := params["eventIds"].([]interface{}); ok {
		ids := make([]string, 0, len(raw))
		for _, item := range raw {
			switch v := item.(type) {
			case float64:
				ids = append(ids, strconv.Itoa(int(v)))
			case string:
				if s := strings.TrimSpace(v); s != "" {
					ids = append(ids, s)
				}
			}
		}
		eventIDs = strings.Join(ids, ",")
	}

	return fmt.Sprintf("%s:%s:%s::", name, level, eventIDs), nil
}

// startEtwWatcher parses the provider spec, opens a real-time ETW session
// scoped to just that provider, and starts a consumer goroutine that treats
// every event reaching it (already kernel-filtered by EventID if the watch
// configured one) as a debounce-worthy signal. Returns nil (logs and does
// not start anything) on any failure — same "best-effort, let the next
// poll cycle retry" philosophy as startRegistryWatcher.
func startEtwWatcher(key string, spec string, onFire func()) *etwWatch {
	prov, err := etw.ParseProvider(spec)
	if err != nil {
		log.Printf("Event watch %q: could not resolve ETW provider %q — not starting: %v", key, spec, err)
		return nil
	}

	// One session per watch, named after the watch key so concurrent
	// sessions (multiple ETW watches at once) never collide on session name
	// — ETW session names must be unique system-wide.
	session := etw.NewRealTimeSession("ApplivierySOAR_" + key)
	if err := session.EnableProvider(prov); err != nil {
		log.Printf("Event watch %q: could not enable ETW provider %q — not starting: %v", key, prov.Name, err)
		session.Stop()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	consumer := etw.NewRealTimeConsumer(ctx)
	consumer.FromSessions(session)

	// Drains consumer.Events for as long as the consumer is running; Stop()
	// cancels ctx and closes the trace handles, which in turn closes the
	// Events channel and lets this goroutine return on its own — no
	// separate stop signal needed here (unlike the registry watcher, which
	// has to poll a stopChan itself because WaitForSingleObject has no
	// channel-aware equivalent).
	go func() {
		for range consumer.Events {
			onFire()
		}
	}()

	if err := consumer.Start(); err != nil {
		log.Printf("Event watch %q: could not start ETW consumer for provider %q — not starting: %v", key, prov.Name, err)
		consumer.Stop()
		session.Stop()
		cancel()
		return nil
	}

	return &etwWatch{key: key, spec: spec, session: session, consumer: consumer, cancel: cancel}
}
