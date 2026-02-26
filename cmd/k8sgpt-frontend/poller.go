package main

import (
	"log"
	"sync"
	"time"
)

// seenUIDs tracks which Result UIDs have already been notified.
// Access is protected by seenMu.
var (
	seenUIDs = map[string]struct{}{}
	seenMu   sync.Mutex
)

// runPoller is started as a goroutine from main. It polls immediately on
// startup (seeding the seen-UID map without sending notifications), then
// polls on each ticker interval and sends notifications for new results.
func runPoller(clients *Clients, namespace, appriseURL string, interval time.Duration) {
	firstPoll := true
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		poll(clients, namespace, appriseURL, firstPoll)
		firstPoll = false

		<-ticker.C
	}
}

func poll(clients *Clients, namespace, appriseURL string, seed bool) {
	results, err := fetchResults(clients, namespace)
	if err != nil {
		log.Printf("poller: fetchResults error: %v", err)
		return
	}

	seenMu.Lock()
	defer seenMu.Unlock()

	for _, r := range results {
		if _, already := seenUIDs[r.UID]; already {
			continue
		}
		seenUIDs[r.UID] = struct{}{}

		if seed {
			// First poll: record UIDs but do not send notifications.
			continue
		}

		if err := sendNotification(appriseURL, r); err != nil {
			log.Printf("poller: sendNotification error for %s: %v", r.UID, err)
		} else {
			log.Printf("poller: notified for result %s (%s %s)", r.UID, r.Kind, r.Name)
		}
	}
}
