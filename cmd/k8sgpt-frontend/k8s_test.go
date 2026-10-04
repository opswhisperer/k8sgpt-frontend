package main

import (
	"context"
	"reflect"
	"testing"
)

func TestParseDetails(t *testing.T) {
	cases := []struct {
		name, in, problem string
		steps             []string
	}{
		{
			"k8sgpt format",
			"Error: Pods are using the default service account.\n\nSolution: 1) Create a dedicated service account. 2) Update the pod/deployment to use it. 3) Set `serviceAccountName`. 4) Redeploy and verify.",
			"Pods are using the default service account.",
			[]string{"Create a dedicated service account.", "Update the pod/deployment to use it.", "Set `serviceAccountName`.", "Redeploy and verify."},
		},
		{"dot numbering", "Error: x\nSolution:\n1. a\n2. b", "x", []string{"a", "b"}},
		{"single step", "Error: x\n\nSolution: just restart it", "x", []string{"just restart it"}},
		{"free text", "Something odd happened.", "Something odd happened.", nil},
		{"empty", "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, s := parseDetails(c.in)
			if p != c.problem || !reflect.DeepEqual(s, c.steps) {
				t.Errorf("got %q %q, want %q %q", p, s, c.problem, c.steps)
			}
		})
	}
}

func TestKubeResultsNormalises(t *testing.T) {
	kube, _ := newFakeKube(t,
		resultObj("demodefault", "Security/ServiceAccount", "demo/default", "Default service account is being used"),
		resultObj("pvc123", "Storage/PersistentVolume", "pvc-123", "small capacity"),
		resultObj("appsweb", "Pod", "apps/web"),
	)
	results, err := kube.Results(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results", len(results))
	}
	byKey := map[string]Result{}
	for _, r := range results {
		byKey[r.Key] = r
	}
	sa, ok := byKey["Security/ServiceAccount/demo/default"]
	if !ok {
		t.Fatalf("keys: %v", sortedKeys(byKey))
	}
	if sa.Namespace != "demo" || sa.Name != "default" || sa.Category != "Security" || sa.BaseKind != "ServiceAccount" ||
		sa.ResultName != "demodefault" || sa.Lifecycle != "historical" || sa.Created == nil {
		t.Errorf("service account result = %+v", sa)
	}
	if sa.Problem != "Something is wrong." || len(sa.Solution) != 2 {
		t.Errorf("details parse: %q %q", sa.Problem, sa.Solution)
	}
	pv := byKey["Storage/PersistentVolume//pvc-123"]
	if pv.Namespace != "" || pv.Name != "pvc-123" {
		t.Errorf("cluster-scoped result = %+v", pv)
	}
	if pod := byKey["Pod/apps/web"]; pod.Category != "" || pod.BaseKind != "Pod" || pod.Errors == nil {
		t.Errorf("pod result = %+v", pod)
	}
}

func TestResultFromKey(t *testing.T) {
	cases := map[string][3]string{
		"Security/ServiceAccount/demo/default": {"Security/ServiceAccount", "demo", "default"},
		"Storage/PersistentVolume//pvc-1":      {"Storage/PersistentVolume", "", "pvc-1"},
		"Pod/apps/web":                         {"Pod", "apps", "web"},
	}
	for key, want := range cases {
		r := resultFromKey(key)
		if got := [3]string{r.Kind, r.Namespace, r.Name}; got != want {
			t.Errorf("%s: got %v, want %v", key, got, want)
		}
	}
}
