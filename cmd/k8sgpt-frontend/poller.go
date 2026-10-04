package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// Options configures the App.
type Options struct {
	Namespace      string
	PollInterval   time.Duration
	NotifyDelay    time.Duration
	HealthWindow   time.Duration
	NotifyResolved bool
	ReadOnly       bool
}

// App polls the cluster, keeps the latest snapshot for the API, and decides
// what to notify.
type App struct {
	kube     *Kube
	store    *Store
	notifier *Notifier
	cfgRules []Rule
	opts     Options
	now      func() time.Time

	refresh chan struct{}

	mu        sync.RWMutex
	results   []Result
	health    Health
	lastPoll  *time.Time
	pollErr   string
	ready     bool
	firstPoll bool // true until the first poll after a start without saved state has run
}

func newApp(kube *Kube, store *Store, notifier *Notifier, cfgRules []Rule, opts Options) *App {
	return &App{
		kube: kube, store: store, notifier: notifier, cfgRules: cfgRules, opts: opts,
		now:       func() time.Time { return time.Now().UTC() },
		refresh:   make(chan struct{}, 1),
		firstPoll: !store.HadState(),
	}
}

// Run polls immediately and then on every interval or refresh request.
func (a *App) Run(ctx context.Context) {
	ticker := time.NewTicker(a.opts.PollInterval)
	defer ticker.Stop()
	for {
		a.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.refresh:
		}
	}
}

// RequestRefresh asks for a poll as soon as possible.
func (a *App) RequestRefresh() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

// rules returns dashboard rules followed by config rules (precedence order).
func (a *App) rules() []Rule {
	return append(a.store.Rules(), a.cfgRules...)
}

// Items applies the current ignores and rules to the latest snapshot.
func (a *App) Items() ([]Item, []RuleView, []Ignore) {
	a.mu.RLock()
	results := a.results
	a.mu.RUnlock()
	items, views, stale := applyRules(results, a.store.Ignores(), a.rules(), a.now())
	seen := a.store.FirstSeen()
	for i := range items {
		// The Result's creation time predates this dashboard's own observation
		// when the finding existed before it started.
		if t, ok := seen[items[i].Key]; ok {
			t := t
			items[i].FirstSeen = &t
		}
		if c := items[i].Created; c != nil && (items[i].FirstSeen == nil || c.Before(*items[i].FirstSeen)) {
			items[i].FirstSeen = c
		}
	}
	return items, views, stale
}

// Snapshot returns the latest results and when they were read.
func (a *App) Snapshot() ([]Result, *time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.results, a.lastPoll
}

func (a *App) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ready
}

// Health returns the latest health snapshot with live frontend fields.
func (a *App) Health() Health {
	a.mu.RLock()
	h := a.health
	a.mu.RUnlock()
	h.Frontend.Notifier = a.notifier.Status()
	h.summarise(a.opts.Namespace, nil)
	if h.CheckedAt.IsZero() {
		h.Status, h.Summary = StatusDegraded, "waiting for the first poll"
	}
	return h
}

func (a *App) Poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := a.now()

	results, err := a.kube.Results(ctx)
	instances, instErr := checkInstances(ctx, a.kube, results, a.opts.HealthWindow, now)

	h := Health{CheckedAt: now, Instances: instances}
	h.Frontend = FrontendHealth{
		Version:       version,
		PollInterval:  a.opts.PollInterval.String(),
		HealthWindow:  a.opts.HealthWindow.String(),
		StoreWritable: a.store.Writable(),
		ReadOnly:      a.opts.ReadOnly,
	}

	a.mu.Lock()
	if err != nil {
		log.Printf("poll: list results: %v", err)
		a.pollErr = err.Error()
	} else {
		a.results, a.pollErr, a.ready = results, "", true
		a.lastPoll = &now
	}
	h.Frontend.LastPoll, h.Frontend.LastPollError = a.lastPoll, a.pollErr
	if instErr != nil {
		log.Printf("poll: %v", instErr)
		h.Frontend.LastPollError = joinErr(h.Frontend.LastPollError, instErr.Error())
	}
	a.health = h
	a.mu.Unlock()

	if err == nil {
		a.notifyFindings(results, now)
	}
	a.notifyHealth(a.Health())
}

func joinErr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// notifyFindings works out which findings are due and sends one batched message.
// A finding is due once it has been present for NotifyDelay and has not been
// notified. Hidden findings are marked as handled without notifying, and keys
// that disappear are forgotten so a recurrence notifies again.
func (a *App) notifyFindings(results []Result, now time.Time) {
	items, _, _ := applyRules(results, a.store.Ignores(), a.rules(), now)
	byKey := make(map[string]Result, len(results))
	for _, r := range results {
		byKey[r.Key] = r
	}

	// The first batch after a start without saved state is framed as "already
	// present"; that framing sticks until the batch is delivered.
	existing := a.firstPoll

	var due, resolved []Result
	var dueKeys []string
	err := a.store.UpdateState(func(st *State) {
		for key := range st.FirstSeen {
			if _, ok := byKey[key]; ok {
				continue
			}
			if _, notified := st.Notified[key]; notified && !st.Suppressed[key] {
				resolved = append(resolved, resultFromKey(key))
			}
			delete(st.FirstSeen, key)
			delete(st.Notified, key)
			delete(st.Suppressed, key)
		}
		for _, it := range items {
			first, ok := st.FirstSeen[it.Key]
			if !ok {
				first = now
				if existing {
					// Present at first start: due at once, reported as "already present".
					first = now.Add(-a.opts.NotifyDelay)
				}
				st.FirstSeen[it.Key] = first
			}
			if _, done := st.Notified[it.Key]; done {
				continue
			}
			if it.Hidden != nil {
				st.Notified[it.Key], st.Suppressed[it.Key] = now, true
				continue
			}
			if now.Sub(first) >= a.opts.NotifyDelay {
				due = append(due, it.Result)
				dueKeys = append(dueKeys, it.Key)
			}
		}
	})
	if err != nil {
		log.Printf("poll: %v", err)
	}

	if len(due) == 0 {
		a.firstPoll = false
	} else {
		sent := true
		if a.notifier.Enabled() {
			log.Printf("notify: %d finding(s)", len(due))
			if err := a.notifier.NotifyIssues(due, existing); err != nil {
				log.Printf("notify: %v (will retry next poll)", err)
				sent = false
			}
		}
		if sent {
			a.firstPoll = false
			_ = a.store.UpdateState(func(st *State) {
				for _, k := range dueKeys {
					st.Notified[k] = now
				}
			})
		}
	}
	if len(resolved) > 0 && a.opts.NotifyResolved && a.notifier.Enabled() {
		if err := a.notifier.NotifyResolved(resolved); err != nil {
			log.Printf("notify resolved: %v", err)
		}
	}
}

// resultFromKey rebuilds enough of a Result to describe a resolved finding.
// Keys are kind/namespace/name; kind may contain "/" ("Security/Pod") and
// namespace is empty for cluster-scoped objects.
func resultFromKey(key string) Result {
	r := Result{Key: key, Name: key}
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		r.Name = key[i+1:]
		rest := key[:i]
		if j := strings.LastIndexByte(rest, '/'); j >= 0 {
			r.Kind, r.Namespace = rest[:j], rest[j+1:]
		} else {
			r.Kind = rest
		}
	}
	return r
}

// notifyHealth sends an alert when analysis starts failing and a recovery
// notice when it stops. The last alerted status is persisted, so neither
// repeats across restarts.
func (a *App) notifyHealth(h Health) {
	failing := false
	for _, in := range h.Instances {
		if in.Status == StatusFailing {
			failing = true
		}
	}
	prev := a.store.HealthState()
	wasFailing := prev.Status == StatusFailing
	if failing == wasFailing {
		return
	}
	if a.notifier.Enabled() {
		if err := a.notifier.NotifyHealth(h, !failing); err != nil {
			log.Printf("notify health: %v (will retry next poll)", err)
			return
		}
	}
	status := StatusOK
	if failing {
		status = StatusFailing
	}
	log.Printf("health: %s → %s: %s", prev.Status, status, h.Summary)
	_ = a.store.UpdateState(func(st *State) {
		st.Health = HealthState{Status: status, Since: h.CheckedAt, Summary: h.Summary}
	})
}
