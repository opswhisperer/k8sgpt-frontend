package main

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynfake "k8s.io/client-go/dynamic/fake"
)

type pollerEnv struct {
	t     *testing.T
	dir   string
	dyn   *dynfake.FakeDynamicClient
	kube  *Kube
	sink  *appriseSink
	clock time.Time
}

func newPollerEnv(t *testing.T) *pollerEnv {
	e := &pollerEnv{t: t, dir: t.TempDir(), sink: newAppriseSink(t), clock: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	e.kube, e.dyn = newFakeKube(t,
		k8sgptObj("k8sgpt", true),
		deploymentObj("k8sgpt", 1, 1),
		resultObj("appsweb", "Pod", "apps/web", "crashloop"),
	)
	return e
}

// app builds an App over the env's data dir, as if the process had just started.
func (e *pollerEnv) app(delay time.Duration) *App {
	n := &Notifier{URL: e.sink.URL}
	a := newApp(e.kube, openStore(e.dir), n, nil, Options{Namespace: testNS, PollInterval: time.Minute, NotifyDelay: delay, HealthWindow: 45 * time.Minute})
	a.now = func() time.Time { return e.clock }
	return a
}

func (e *pollerEnv) poll(a *App, advance time.Duration) []apprisePayload {
	e.clock = e.clock.Add(advance)
	before := len(e.sink.all())
	a.Poll(context.Background())
	return e.sink.all()[before:]
}

func (e *pollerEnv) addResult(obj *unstructured.Unstructured) {
	gvr := defaultGroupVersion.WithResource("results")
	if _, err := e.dyn.Resource(gvr).Namespace(testNS).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *pollerEnv) deleteResult(name string) {
	gvr := defaultGroupVersion.WithResource("results")
	if err := e.dyn.Resource(gvr).Namespace(testNS).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func titles(ps []apprisePayload) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Title)
	}
	return out
}

func TestPollerNotificationLifecycle(t *testing.T) {
	e := newPollerEnv(t)
	a := e.app(5 * time.Minute)

	// First start: existing findings are summarised once.
	got := e.poll(a, 0)
	if len(got) != 1 || got[0].Title != "K8sGPT: 1 open issue" || !strings.Contains(got[0].Body, "already present") {
		t.Fatalf("first poll sent %v", titles(got))
	}
	if !a.Ready() {
		t.Error("app not ready after a successful poll")
	}

	// Nothing new: nothing sent.
	if got := e.poll(a, time.Minute); len(got) != 0 {
		t.Fatalf("idle poll sent %v", titles(got))
	}

	// A new finding waits for the delay, then notifies once.
	e.addResult(resultObj("appsapi", "Pod", "apps/api", "oom"))
	if got := e.poll(a, time.Minute); len(got) != 0 {
		t.Fatalf("notified before delay: %v", titles(got))
	}
	got = e.poll(a, 5*time.Minute)
	if len(got) != 1 || got[0].Title != "K8sGPT: Pod apps/api" {
		t.Fatalf("after delay sent %v", titles(got))
	}

	// A finding that clears before the delay never notifies.
	e.addResult(resultObj("appsblip", "Pod", "apps/blip", "blip"))
	e.poll(a, time.Minute)
	e.deleteResult("appsblip")
	if got := e.poll(a, 10*time.Minute); len(got) != 0 {
		t.Fatalf("transient finding notified: %v", titles(got))
	}

	// Restart: state is persisted, nothing is re-sent.
	a2 := e.app(5 * time.Minute)
	if got := e.poll(a2, time.Minute); len(got) != 0 {
		t.Fatalf("restart re-sent %v", titles(got))
	}

	// Clears and recurs: notifies again.
	e.deleteResult("appsapi")
	e.poll(a2, time.Minute)
	e.addResult(resultObj("appsapi", "Pod", "apps/api", "oom again"))
	e.poll(a2, time.Minute)
	got = e.poll(a2, 5*time.Minute)
	if len(got) != 1 || got[0].Title != "K8sGPT: Pod apps/api" {
		t.Fatalf("recurrence sent %v", titles(got))
	}
}

func TestPollerHiddenFindingsDoNotNotify(t *testing.T) {
	e := newPollerEnv(t)
	a := e.app(0)
	e.poll(a, 0) // initial summary

	if _, err := a.store.AddRule(Rule{Namespace: "kube-*"}); err != nil {
		t.Fatal(err)
	}
	e.addResult(resultObj("kubesystemproxy", "Security/Pod", "kube-system/kube-proxy", "privileged"))
	if got := e.poll(a, time.Minute); len(got) != 0 {
		t.Fatalf("hidden finding notified: %v", titles(got))
	}
	// Removing the rule later does not suddenly notify it.
	rules := a.store.Rules()
	_, _ = a.store.DeleteRule(rules[0].ID)
	if got := e.poll(a, time.Minute); len(got) != 0 {
		t.Fatalf("unhidden finding notified: %v", titles(got))
	}
}

func TestPollerRetriesFailedSend(t *testing.T) {
	e := newPollerEnv(t)
	a := e.app(0)
	e.sink.setStatus(500)
	e.poll(a, 0)
	e.sink.setStatus(200)
	got := e.poll(a, time.Minute)
	found := false
	for _, p := range got {
		if strings.Contains(p.Title, "open issue") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed send was not retried: %v", titles(got))
	}
}

func TestPollerHealthAlertsOnceAndRecovers(t *testing.T) {
	e := newPollerEnv(t)
	a := e.app(0)
	e.poll(a, 0) // summary; healthy, no alert

	ev := eventObj("fail", "k8sgpt", msg401, e.clock.Add(time.Minute), 1)
	if _, err := e.dyn.Resource(eventsGVR).Namespace(testNS).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got := e.poll(a, time.Minute)
	if len(got) != 1 || got[0].Type != "failure" {
		t.Fatalf("expected one failure alert, got %v", titles(got))
	}
	if got := e.poll(a, time.Minute); len(got) != 0 {
		t.Fatalf("alert repeated: %v", titles(got))
	}
	// Restart while still failing: no repeat.
	a2 := e.app(0)
	if got := e.poll(a2, time.Minute); len(got) != 0 {
		t.Fatalf("alert repeated after restart: %v", titles(got))
	}
	if h := a2.Health(); h.Status != StatusFailing {
		t.Errorf("health = %s", h.Status)
	}
	// The failure ages out of the window: recovery notice, once.
	got = e.poll(a2, time.Hour)
	if len(got) != 1 || got[0].Type != "success" {
		t.Fatalf("expected one recovery notice, got %v", titles(got))
	}
	if got := e.poll(a2, time.Minute); len(got) != 0 {
		t.Fatalf("recovery repeated: %v", titles(got))
	}
}
