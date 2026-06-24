package main

import (
	"log"
	"sync"
	"time"
)

// seenUIDs tracks Result UIDs that have already been notified (never notify again).
// pendingUIDs tracks UIDs first seen but not yet past the notify delay; mapped to
// the time they were first observed.  Both are protected by seenMu.
var (
	seenUIDs    = map[string]struct{}{}
	pendingUIDs = map[string]time.Time{}
	seenMu      sync.Mutex
)

// runPoller is started as a goroutine from main. It polls immediately on
// startup and then continues on each ticker interval.
func runPoller(clients *Clients, namespace, appriseURL, uiURL string, interval, delay time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		poll(clients, namespace, appriseURL, uiURL, delay)
		<-ticker.C
	}
}

func poll(clients *Clients, namespace, appriseURL, uiURL string, delay time.Duration) {
	results, err := fetchResults(clients, namespace)
	if err != nil {
		log.Printf("poller: fetchResults error: %v", err)
		return
	}

	// Build a set of UIDs currently present.
	currentUIDs := make(map[string]struct{}, len(results))
	for _, r := range results {
		currentUIDs[r.UID] = struct{}{}
	}

	seenMu.Lock()
	defer seenMu.Unlock()

	// Drop pending UIDs that are no longer present — transient issue cleared.
	for uid := range pendingUIDs {
		if _, present := currentUIDs[uid]; !present {
			log.Printf("poller: result %s cleared before notify delay elapsed — suppressing notification", uid)
			delete(pendingUIDs, uid)
		}
	}

	now := time.Now()

	var ready []Result
	for _, r := range results {
		if _, already := seenUIDs[r.UID]; already {
			continue
		}

		firstSeen, pending := pendingUIDs[r.UID]
		if !pending {
			// New issue: start the timer.
			pendingUIDs[r.UID] = now
			log.Printf("poller: result %s (%s %s) first seen — waiting %s before notifying", r.UID, r.Kind, r.Name, delay)
			continue
		}

		if now.Sub(firstSeen) < delay {
			// Still within the delay window.
			continue
		}

		// Delay elapsed and issue is still present: mark and collect.
		delete(pendingUIDs, r.UID)
		seenUIDs[r.UID] = struct{}{}
		ready = append(ready, r)
		log.Printf("poller: result %s (%s %s) ready to notify", r.UID, r.Kind, r.Name)
	}

	if len(ready) == 0 {
		return
	}

	log.Printf("poller: sending summary notification for %d result(s)", len(ready))
	if err := sendBatchNotification(appriseURL, ready, uiURL); err != nil {
		log.Printf("poller: sendBatchNotification error: %v", err)
	}
}
