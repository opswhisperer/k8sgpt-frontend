package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})? (\S+)$`)

// scrape fetches /metrics from a metrics server's handler and returns the
// samples keyed by `name{labels}`, plus the raw text.
func scrape(t *testing.T, app *App) (map[string]float64, string) {
	t.Helper()
	srv := httptest.NewServer(metricsServer(":0", app).Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("GET /metrics: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	b, _ := io.ReadAll(resp.Body)
	return parseMetrics(t, string(b)), string(b)
}

func parseMetrics(t *testing.T, text string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	typed := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		if f := strings.Fields(line); len(f) >= 4 && f[0] == "#" && f[1] == "TYPE" {
			typed[f[2]] = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("malformed sample %q", line)
		}
		if !typed[m[1]] {
			t.Errorf("sample %q has no TYPE line before it", line)
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("sample %q: %v", line, err)
		}
		key := m[1]
		if m[2] != "" {
			key += "{" + m[2] + "}"
		}
		out[key] = v
	}
	return out
}

func TestMetricsMatchSummary(t *testing.T) {
	fresh := resultObj("appsapi", "Pod", "apps/api", "pending")
	fresh.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour)))
	kube, _ := newFakeKube(t, k8sgptObj("k8sgpt", true), deploymentObj("k8sgpt", 1, 1),
		resultObj("appsweb", "Pod", "apps/web", "crashloop"),
		fresh,
		resultObj("svcweb", "Service", "apps/web", "no endpoints"),
		resultObj("demodefault", "Security/ServiceAccount", "demo/default", "default SA"),
		resultObj("demobuilder", "Security/ServiceAccount", "demo/builder", "default SA"),
		resultObj("toolsjob", "Pod", "tools/job", "error"))
	cfgRules := []Rule{mustRule(t, Rule{ID: "config-1", Namespace: "demo", From: "config"})}
	app := newApp(kube, openStore(t.TempDir()), &Notifier{}, cfgRules, Options{
		Namespace: testNS, PollInterval: time.Minute, NotifyDelay: time.Minute, HealthWindow: time.Hour,
	})
	app.Poll(context.Background())
	now := time.Now().UTC()
	if err := app.store.AddIgnores([]Ignore{
		{Key: "Pod/tools/job", Kind: "Pod", Namespace: "tools", Name: "job", At: now},
		{Key: "Pod/gone/old", Kind: "Pod", Namespace: "gone", Name: "old", At: now}, // stale
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerHandlers(mux, app)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sum := getResults(t, srv.URL).Summary
	if sum.Visible != 3 || sum.HiddenByRule != 2 || sum.HiddenByIgnore != 1 || sum.New24h != 1 {
		t.Fatalf("fixture should give distinct counts for each state: %+v", sum)
	}
	m, _ := scrape(t, app)
	want := map[string]float64{
		`k8sgpt_frontend_findings{state="visible"}`:       float64(sum.Visible),
		`k8sgpt_frontend_findings{state="hidden_rule"}`:   float64(sum.HiddenByRule),
		`k8sgpt_frontend_findings{state="hidden_ignore"}`: float64(sum.HiddenByIgnore),
		`k8sgpt_frontend_new_findings_24h`:                float64(sum.New24h),
		`k8sgpt_frontend_stale_ignores`:                   1,
		`k8sgpt_frontend_last_poll_success`:               1,
	}
	for c, n := range sum.ByCategory {
		want[`k8sgpt_frontend_visible_findings{category="`+c+`"}`] = float64(n)
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("%s = %v (present %t), want %v", k, got, ok, v)
		}
	}
	var states, cats float64
	for k, v := range m {
		switch {
		case strings.HasPrefix(k, "k8sgpt_frontend_findings{"):
			states += v
		case strings.HasPrefix(k, "k8sgpt_frontend_visible_findings{"):
			cats += v
		}
	}
	if states != float64(sum.Total) || cats != float64(sum.Visible) {
		t.Errorf("states add up to %v (total %d), categories to %v (visible %d)", states, sum.Total, cats, sum.Visible)
	}
}

func TestMetricsFailedPoll(t *testing.T) {
	kube, dyn := newFakeKube(t, k8sgptObj("k8sgpt", true), deploymentObj("k8sgpt", 1, 1),
		resultObj("appsweb", "Pod", "apps/web", "crashloop"))
	app := newApp(kube, openStore(t.TempDir()), &Notifier{}, nil, Options{
		Namespace: testNS, PollInterval: time.Minute, NotifyDelay: time.Minute, HealthWindow: time.Hour,
	})

	m, _ := scrape(t, app)
	if m["k8sgpt_frontend_last_poll_success"] != 0 || m["k8sgpt_frontend_last_poll_timestamp_seconds"] != 0 {
		t.Fatalf("before the first poll: %v", m)
	}

	first := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return first }
	app.Poll(context.Background())
	m, _ = scrape(t, app)
	if m["k8sgpt_frontend_last_poll_success"] != 1 || m["k8sgpt_frontend_last_poll_timestamp_seconds"] != float64(first.Unix()) {
		t.Fatalf("after a good poll: %v", m)
	}

	dyn.PrependReactor("list", "results", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("results is forbidden: User cannot list resource")
	})
	app.now = func() time.Time { return first.Add(time.Minute) }
	app.Poll(context.Background())
	m, text := scrape(t, app)
	if m["k8sgpt_frontend_last_poll_success"] != 0 {
		t.Errorf("last_poll_success = %v after a failed poll", m["k8sgpt_frontend_last_poll_success"])
	}
	if m["k8sgpt_frontend_last_poll_timestamp_seconds"] != float64(first.Unix()) {
		t.Errorf("last_poll_timestamp_seconds = %v, want the last good poll %d", m["k8sgpt_frontend_last_poll_timestamp_seconds"], first.Unix())
	}
	if m[`k8sgpt_frontend_findings{state="visible"}`] != 1 {
		t.Errorf("the last good results should still be counted: %v", m)
	}
	if strings.Contains(text, "forbidden") {
		t.Errorf("error text leaked into metrics:\n%s", text)
	}
}

func TestMetricsLabelsBounded(t *testing.T) {
	kube, _ := newFakeKube(t, k8sgptObj("k8sgpt", true), deploymentObj("k8sgpt", 1, 1),
		resultObj("r1", "Pod", "tenant-a/payroll-db-0", msg401),
		resultObj("r2", "Security/ServiceAccount", "tenant-b/builder", "secret token in use"),
		resultObj("r3", "Node", "worker-7", "disk pressure"))
	app := newApp(kube, openStore(t.TempDir()), &Notifier{}, nil, Options{
		Namespace: testNS, PollInterval: time.Minute, NotifyDelay: time.Minute, HealthWindow: time.Hour,
	})
	app.Poll(context.Background())

	m, text := scrape(t, app)
	allowed := map[string]bool{"state": true, "category": true, "version": true}
	labelName := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="`)
	for k := range m {
		i := strings.IndexByte(k, '{')
		if i < 0 {
			continue
		}
		for _, l := range labelName.FindAllStringSubmatch(k[i:], -1) {
			if !allowed[l[1]] {
				t.Errorf("unexpected label %q in %s", l[1], k)
			}
		}
	}
	for _, s := range []string{"tenant-a", "payroll-db-0", "tenant-b", "builder", "worker-7", "sk-proj", "secret token", "disk pressure", "k8sgpt-sample-secret"} {
		if strings.Contains(text, s) {
			t.Errorf("metrics contain %q:\n%s", s, text)
		}
	}
	if m[`k8sgpt_frontend_visible_findings{category="Security"}`] != 1 || m[`k8sgpt_frontend_visible_findings{category="Pod"}`] != 1 {
		t.Errorf("categories: %v", m)
	}
}

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Errorf("escapeLabel = %q", got)
	}
}

func TestMetricsListenEmptyDisables(t *testing.T) {
	kube, _ := newFakeKube(t)
	app := newApp(kube, openStore(""), &Notifier{}, nil, Options{Namespace: testNS, PollInterval: time.Minute})
	if srv := metricsServer("", app); srv != nil {
		t.Fatalf("metricsServer(\"\") = %+v, want nil", srv)
	}
}

// With login on, /metrics is still served on its own listener without a
// session, and only there: the main server has no /metrics and the metrics
// listener serves nothing else.
func TestMetricsOwnListenerOutsideOIDC(t *testing.T) {
	idp := newFakeIdP(t)
	kube, _ := newFakeKube(t, k8sgptObj("k8sgpt", true), deploymentObj("k8sgpt", 1, 1),
		resultObj("appsweb", "Pod", "apps/web", "crashloop"))
	app := newApp(kube, openStore(t.TempDir()), &Notifier{}, nil, Options{
		Namespace: testNS, PollInterval: time.Minute, NotifyDelay: time.Minute, HealthWindow: time.Hour,
	})
	app.Poll(context.Background())

	mux := http.NewServeMux()
	registerHandlers(mux, app)
	auth, err := newAuth(OIDCConfig{Issuer: idp.issuer(), ClientID: "k8sgpt", ClientSecret: "s3cret",
		RedirectURL: "http://127.0.0.1/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	auth.Register(mux)
	main := httptest.NewServer(auth.Middleware(mux))
	t.Cleanup(main.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	msrv := metricsServer(ln.Addr().String(), app)
	go func() { _ = msrv.Serve(ln) }()
	t.Cleanup(func() { _ = msrv.Close() })
	base := "http://" + ln.Addr().String()

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, body := get(t, noRedirect, base+"/metrics")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `k8sgpt_frontend_findings{state="visible"} 1`) {
		t.Fatalf("metrics listener without a session: %d\n%s", resp.StatusCode, body)
	}
	for _, p := range []string{"/", "/api/results", "/api/health", "/healthz"} {
		if resp, _ := get(t, noRedirect, base+p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("metrics listener GET %s = %d, want 404", p, resp.StatusCode)
		}
	}
	if resp, _ := get(t, noRedirect, main.URL+"/metrics"); resp.StatusCode == http.StatusOK {
		t.Errorf("main listener serves /metrics without a session")
	}
}
