package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, readOnly bool) (*httptest.Server, *App) {
	kube, _ := newFakeKube(t,
		k8sgptObj("k8sgpt", true),
		deploymentObj("k8sgpt", 1, 1),
		resultObj("appsweb", "Pod", "apps/web", "crashloop"),
		resultObj("demodefault", "Security/ServiceAccount", "demo/default", "default SA"),
		resultObj("appsdefault", "Security/ServiceAccount", "apps/default", "default SA"),
	)
	cfgRules := []Rule{mustRule(t, Rule{ID: "config-1", Namespace: "demo", From: "config"})}
	app := newApp(kube, openStore(t.TempDir()), &Notifier{}, cfgRules, Options{
		Namespace: testNS, PollInterval: time.Minute, NotifyDelay: time.Minute, HealthWindow: time.Hour, ReadOnly: readOnly,
	})
	app.Poll(context.Background())
	mux := http.NewServeMux()
	registerHandlers(mux, app)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, app
}

func postJSON(t *testing.T, url, body string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getResults(t *testing.T, base string) resultsResponse {
	t.Helper()
	resp, err := http.Get(base + "/api/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out resultsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAPIIgnoreAndRuleFlow(t *testing.T) {
	srv, _ := newTestServer(t, false)

	r := getResults(t, srv.URL)
	if r.Summary.Total != 3 || r.Summary.Hidden != 1 || !r.Writable { // config rule hides demo/default
		t.Fatalf("initial summary = %+v writable=%v", r.Summary, r.Writable)
	}

	code, body := postJSON(t, srv.URL+"/api/ignore", `{"keys":["Pod/apps/web","Pod/nope/x"],"note":"known"}`)
	if code != 200 || body["ignored"].(float64) != 1 || len(body["missing"].([]interface{})) != 1 {
		t.Fatalf("ignore: %d %v", code, body)
	}
	code, body = postJSON(t, srv.URL+"/api/rules", `{"rule":{"kind":"ServiceAccount"},"note":"fine"}`)
	if code != 200 {
		t.Fatalf("add rule: %d %v", code, body)
	}
	ruleID := body["rule"].(map[string]interface{})["id"].(string)

	r = getResults(t, srv.URL)
	if r.Summary.Hidden != 3 || r.Summary.Visible != 0 {
		t.Fatalf("after hiding: %+v", r.Summary)
	}
	for _, it := range r.Items {
		if it.Key == "Pod/apps/web" && (it.Hidden == nil || it.Hidden.By != "ignore" || it.Hidden.Note != "known") {
			t.Errorf("web hidden = %+v", it.Hidden)
		}
	}

	if code, _ := postJSON(t, srv.URL+"/api/rules", `{"rule":{"name_regex":"("}}`); code != 400 {
		t.Errorf("bad regex: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/rules", `{"rule":{}}`); code != 400 {
		t.Errorf("empty rule: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/rules/delete", `{"id":"config-1"}`); code != 400 {
		t.Errorf("deleting config rule: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/rules/delete", `{"id":"`+ruleID+`"}`); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/rules/delete", `{"id":"`+ruleID+`"}`); code != 404 {
		t.Errorf("second delete: %d", code)
	}
	if code, body := postJSON(t, srv.URL+"/api/unignore", `{"keys":["Pod/apps/web"]}`); code != 200 || body["unignored"].(float64) != 1 {
		t.Errorf("unignore: %d %v", code, body)
	}
	if r := getResults(t, srv.URL); r.Summary.Hidden != 1 {
		t.Errorf("after undo: %+v", r.Summary)
	}
}

func TestAPIGuards(t *testing.T) {
	srv, _ := newTestServer(t, false)
	resp, err := http.Post(srv.URL+"/api/ignore", "text/plain", strings.NewReader(`{"keys":["Pod/apps/web"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST = %d", resp.StatusCode)
	}
	if code, _ := postJSON(t, srv.URL+"/api/ignore", `{"keys":[]}`); code != 400 {
		t.Errorf("empty keys = %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/ignore", `{"keys":["a"],"bogus":1}`); code != 400 {
		t.Errorf("unknown field = %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/test-notification", `{}`); code != 400 {
		t.Errorf("test notification without apprise = %d", code)
	}

	ro, _ := newTestServer(t, true)
	if code, _ := postJSON(t, ro.URL+"/api/ignore", `{"keys":["Pod/apps/web"]}`); code != 403 {
		t.Errorf("read-only ignore = %d", code)
	}
	if code, _ := postJSON(t, ro.URL+"/api/refresh", `{}`); code != 202 {
		t.Errorf("read-only refresh = %d", code)
	}
	if r := getResults(t, ro.URL); r.Writable {
		t.Error("read-only server reports writable")
	}
}

func TestAPIHealthAndProbes(t *testing.T) {
	srv, _ := newTestServer(t, false)
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/": 200, "/nope": 404} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Status != StatusOK || len(h.Instances) != 1 || h.Instances[0].Model != "gpt-5.4-mini" || !h.Frontend.StoreWritable {
		t.Errorf("health = %+v", h)
	}

	raw, err := http.Get(srv.URL + "/api/results?raw=1")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Body.Close()
	var list []Result
	if err := json.NewDecoder(raw.Body).Decode(&list); err != nil || len(list) != 3 {
		t.Errorf("raw results: %v %d", err, len(list))
	}
}
