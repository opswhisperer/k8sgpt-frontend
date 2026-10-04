package main

import (
	"strings"
	"testing"
	"time"
)

func res(kind, ns, name string, errs ...string) Result {
	category, base := "", kind
	if i := strings.LastIndex(kind, "/"); i >= 0 {
		category, base = kind[:i], kind[i+1:]
	}
	return Result{Key: resultKey(kind, ns, name), Kind: kind, Category: category, BaseKind: base, Namespace: ns, Name: name, Errors: errs}
}

func mustRule(t *testing.T, r Rule) Rule {
	t.Helper()
	if err := r.compile(); err != nil {
		t.Fatalf("compile %+v: %v", r, err)
	}
	return r
}

func TestRuleMatch(t *testing.T) {
	sa := res("Security/ServiceAccount", "apps", "default", "Default service account is being used by pods: [a b]")
	pvc := res("Storage/PersistentVolumeClaim", "longhorn-system", "data-1", "PersistentVolumeClaim data-1 has small capacity (100Mi)")
	pv := res("Storage/PersistentVolume", "", "pvc-123")
	pod := res("Pod", "apps", "web-7d9")

	cases := []struct {
		name string
		rule Rule
		hits []Result
		miss []Result
	}{
		{"full kind", Rule{Kind: "Security/ServiceAccount"}, []Result{sa}, []Result{pvc, pod}},
		{"base kind", Rule{Kind: "serviceaccount"}, []Result{sa}, []Result{pvc}},
		{"category glob", Rule{Kind: "Storage/*"}, []Result{pvc, pv}, []Result{sa, pod}},
		{"suffix glob on base kind", Rule{Kind: "*Volume"}, []Result{pv}, []Result{pvc}},
		{"namespace glob", Rule{Namespace: "longhorn-*"}, []Result{pvc}, []Result{sa, pv}},
		{"name regex", Rule{NameRegex: "^web-"}, []Result{pod}, []Result{sa}},
		{"error regex", Rule{ErrorRegex: "small capacity"}, []Result{pvc}, []Result{sa, pv}},
		{"all fields", Rule{Kind: "Pod", Namespace: "apps", NameRegex: "web"}, []Result{pod}, []Result{sa}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustRule(t, c.rule)
			for i := range c.hits {
				if !r.Match(&c.hits[i]) {
					t.Errorf("expected match on %s", c.hits[i].Key)
				}
			}
			for i := range c.miss {
				if r.Match(&c.miss[i]) {
					t.Errorf("unexpected match on %s", c.miss[i].Key)
				}
			}
		})
	}
}

func TestRuleValidation(t *testing.T) {
	bad := []Rule{
		{},
		{NameRegex: "("},
		{ErrorRegex: "[z-a]"},
		{Kind: "["},
		{Namespace: strings.Repeat("a", 201)},
	}
	for _, r := range bad {
		if err := r.compile(); err == nil {
			t.Errorf("expected error for %+v", r)
		}
	}
	r := Rule{Kind: " Pod ", Note: strings.Repeat("n", 600)}
	if err := r.compile(); err != nil {
		t.Fatal(err)
	}
	if r.Kind != "Pod" || len(r.Note) != maxNote {
		t.Errorf("normalisation: kind=%q note=%d", r.Kind, len(r.Note))
	}
}

func TestApplyRulesPrecedenceAndCounts(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	results := []Result{
		res("Security/ServiceAccount", "apps", "default"),
		res("Security/ServiceAccount", "media", "default"),
		res("Pod", "apps", "web"),
		res("Pod", "apps", "worker"),
	}
	ignores := map[string]Ignore{
		results[0].Key:      {Key: results[0].Key, Note: "single"},
		results[3].Key:      {Key: results[3].Key, Until: &past}, // expired
		"Pod/gone/whatever": {Key: "Pod/gone/whatever"},          // stale
	}
	rules := []Rule{
		mustRule(t, Rule{ID: "d1", Kind: "ServiceAccount", From: "dashboard", Until: &future}),
		mustRule(t, Rule{ID: "d2", Kind: "Pod", NameRegex: "^w", From: "dashboard", Until: &past}), // expired
		mustRule(t, Rule{ID: "config-1", Namespace: "apps", From: "config"}),
	}
	items, views, stale := applyRules(results, ignores, rules, now)

	want := []struct {
		by, rule string
	}{
		{"ignore", ""},       // single ignore beats the rule
		{"rule", "d1"},       // dashboard rule
		{"rule", "config-1"}, // config rule
		{"rule", "config-1"}, // expired ignore and expired rule skipped
	}
	for i, w := range want {
		h := items[i].Hidden
		if h == nil || h.By != w.by || h.RuleID != w.rule {
			t.Errorf("item %d (%s): hidden=%+v, want by=%s rule=%s", i, items[i].Key, h, w.by, w.rule)
		}
	}
	if views[0].Matches != 2 { // counts the ignored one too
		t.Errorf("d1 matches = %d, want 2", views[0].Matches)
	}
	if !views[1].Expired || views[1].Matches != 0 {
		t.Errorf("d2 = %+v, want expired with 0 matches", views[1])
	}
	if views[2].Matches != 3 {
		t.Errorf("config-1 matches = %d, want 3", views[2].Matches)
	}
	if len(stale) != 1 || stale[0].Key != "Pod/gone/whatever" {
		t.Errorf("stale = %+v", stale)
	}
}
