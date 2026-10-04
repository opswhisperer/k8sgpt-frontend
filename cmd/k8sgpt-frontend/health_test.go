package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestClassifyError(t *testing.T) {
	cases := map[string]string{
		msg401: "auth",
		"error, status code: 429, status: 429 Too Many Requests, message: You exceeded your current quota, please check your plan and billing details. insufficient_quota": "quota",
		"error, status code: 429, status: 429 Too Many Requests, message: Rate limit reached for gpt-4o":                                                                   "rate_limit",
		"error, status code: 404, message: The model `gpt-9` does not exist or you do not have access to it.":                                                              "model",
		`Post "https://api.openai.com/v1/chat/completions": dial tcp: lookup api.openai.com: no such host`:                                                                 "unreachable",
		"context deadline exceeded": "unreachable",
		"failed to call Analyze RPC: rpc error: code = Unavailable desc = error reading from server: EOF": "server",
		"something else entirely": "other",
	}
	for msg, want := range cases {
		if got, _ := classifyError(msg); got != want {
			t.Errorf("classify(%.60q) = %s, want %s", msg, got, want)
		}
	}
}

func TestScrubMessage(t *testing.T) {
	got := scrubMessage("key sk-proj-abcdefghijklmnopqrstuvwxyz0123 rejected")
	if strings.Contains(got, "abcdefghijkl") || !strings.Contains(got, "sk-proj…") {
		t.Errorf("scrub = %q", got)
	}
}

func TestBuildInstance(t *testing.T) {
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	window := 45 * time.Minute
	cr := k8sgptObj("k8sgpt", true).Object
	recent := []unstructured.Unstructured{*eventObj("e1", "k8sgpt", msg401, now.Add(-10*time.Minute), 53)}
	old := []unstructured.Unstructured{*eventObj("e1", "k8sgpt", msg401, now.Add(-2*time.Hour), 53)}
	other := []unstructured.Unstructured{*eventObj("e2", "someone-else", msg401, now, 1)}
	ready := deploymentObj("k8sgpt", 1, 1)

	in := buildInstance(cr, recent, ready, nil, nil, window, now)
	if in.Status != StatusFailing || in.Analysis.Class != "auth" || in.Analysis.Count != 53 || in.Analysis.Source != "event" {
		t.Errorf("recent failure: %+v", in.Analysis)
	}
	if !strings.Contains(in.Analysis.Hint, "k8sgpt-sample-secret") {
		t.Errorf("hint should name the secret: %q", in.Analysis.Hint)
	}
	if in.Backend != "openai" || in.Model != "gpt-5.4-mini" || in.Version != "v0.4.39" || !in.Anonymized || len(in.Filters) != 2 {
		t.Errorf("config fields: %+v", in)
	}

	in = buildInstance(cr, old, ready, nil, nil, window, now)
	if in.Status != StatusOK || in.Analysis.Status != StatusOK || in.Analysis.LastAt == nil {
		t.Errorf("old failure should be ok with history: %+v", in)
	}

	in = buildInstance(cr, other, ready, nil, nil, window, now)
	if in.Status != StatusOK {
		t.Errorf("event for another CR should not count: %+v", in.Analysis)
	}

	in = buildInstance(k8sgptObj("k8sgpt", false).Object, recent, ready, nil, nil, window, now)
	if in.Analysis.Status != "disabled" || in.Status != StatusOK {
		t.Errorf("AI disabled: %+v", in)
	}

	withStatus := k8sgptObj("k8sgpt", true)
	_ = unstructured.SetNestedField(withStatus.Object, "status code: 429 insufficient_quota", "status", "lastAnalysisError")
	_ = unstructured.SetNestedField(withStatus.Object, now.Add(-time.Minute).Format(time.RFC3339), "status", "lastAnalysisErrorTime")
	in = buildInstance(withStatus.Object, nil, ready, nil, nil, window, now)
	if in.Analysis.Class != "quota" || in.Analysis.Source != "status" {
		t.Errorf("status field: %+v", in.Analysis)
	}

	in = buildInstance(cr, nil, deploymentObj("k8sgpt", 2, 1), nil, nil, window, now)
	if in.Status != StatusDegraded {
		t.Errorf("1/2 ready should be degraded, got %s", in.Status)
	}
	in = buildInstance(cr, nil, deploymentObj("k8sgpt", 1, 0), nil, nil, window, now)
	if in.Status != StatusFailing {
		t.Errorf("0/1 ready should be failing, got %s", in.Status)
	}
}

func TestCheckInstancesAndSummary(t *testing.T) {
	now := time.Now().UTC()
	kube, _ := newFakeKube(t,
		k8sgptObj("k8sgpt", true),
		eventObj("e1", "k8sgpt", msg401, now.Add(-time.Minute), 3),
		deploymentObj("k8sgpt", 1, 1),
	)
	instances, err := checkInstances(context.Background(), kube, nil, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	h := Health{Instances: instances}
	h.summarise(testNS, nil)
	if h.Status != StatusFailing || !strings.Contains(h.Summary, "API key rejected") || !strings.Contains(h.Summary, "gpt-5.4-mini") {
		t.Errorf("health = %s: %s", h.Status, h.Summary)
	}

	empty, _ := newFakeKube(t)
	instances, _ = checkInstances(context.Background(), empty, nil, time.Hour, now)
	h = Health{Instances: instances}
	h.summarise(testNS, nil)
	if h.Status != StatusDegraded || !strings.Contains(h.Summary, "no K8sGPT resource") {
		t.Errorf("no CR: %s: %s", h.Status, h.Summary)
	}
}

func TestBuildInstanceRecoversAfterResultWrite(t *testing.T) {
	now := time.Date(2026, 10, 4, 2, 30, 0, 0, time.UTC)
	cr := k8sgptObj("k8sgpt", true).Object
	fail := now.Add(-20 * time.Minute)
	events := []unstructured.Unstructured{*eventObj("e1", "k8sgpt", msg401, fail, 3)}
	ready := deploymentObj("k8sgpt", 1, 1)

	before := fail.Add(-time.Minute)
	if in := buildInstance(cr, events, ready, nil, &before, time.Hour, now); in.Status != StatusFailing {
		t.Errorf("result written before the failure must not clear it: %+v", in.Analysis)
	}
	after := fail.Add(5 * time.Minute)
	in := buildInstance(cr, events, ready, nil, &after, time.Hour, now)
	if in.Status != StatusOK || in.Analysis.Status != StatusOK || in.Analysis.LastAt == nil || in.Analysis.LastSuccessAt == nil {
		t.Errorf("result written after the failure should clear it, keeping history: %+v", in.Analysis)
	}
}

func TestCheckInstancesUsesOwnedResultWrites(t *testing.T) {
	now := time.Now().UTC()
	kube, _ := newFakeKube(t,
		k8sgptObj("k8sgpt", true),
		eventObj("e1", "k8sgpt", msg401, now.Add(-10*time.Minute), 1),
		deploymentObj("k8sgpt", 1, 1),
	)
	later := now.Add(-time.Minute)
	other := []Result{{Owner: "someone-else", Updated: &later}}
	instances, _ := checkInstances(context.Background(), kube, other, time.Hour, now)
	if instances[0].Status != StatusFailing {
		t.Errorf("another CR's results must not clear this one: %+v", instances[0].Analysis)
	}
	mine := []Result{{Owner: "k8sgpt", Updated: &later}}
	instances, _ = checkInstances(context.Background(), kube, mine, time.Hour, now)
	if instances[0].Status != StatusOK {
		t.Errorf("own result written after the failure should clear it: %+v", instances[0].Analysis)
	}
}
