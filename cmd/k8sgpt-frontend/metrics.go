package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// metricsServer serves /metrics on its own address, outside the OIDC
// middleware, or returns nil when addr is empty. It has no auth, so keep its
// port off any public route.
func metricsServer(addr string, app *App) *http.Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", app.serveMetrics)
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

// serveMetrics writes the Prometheus text format by hand to keep
// client_golang out of the dependencies. The counts are the dashboard tiles'
// (resultsResponse), so the two always agree. Labels stay bounded: a state or
// a category, never a finding's name, namespace or error text.
func (app *App) serveMetrics(w http.ResponseWriter, r *http.Request) {
	resp := app.resultsResponse()
	sum := resp.Summary
	last, ok := app.PollStatus()

	var b bytes.Buffer
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	sample := func(name, labels string, v float64) {
		if labels != "" {
			labels = "{" + labels + "}"
		}
		fmt.Fprintf(&b, "%s%s %s\n", name, labels, strconv.FormatFloat(v, 'f', -1, 64))
	}

	gauge("k8sgpt_frontend_build_info", "Version of k8sgpt-frontend; always 1.")
	sample("k8sgpt_frontend_build_info", `version="`+escapeLabel(version)+`"`, 1)

	gauge("k8sgpt_frontend_findings", "Findings by state: visible, hidden_rule (hidden by a rule) or hidden_ignore (hidden one by one). The states add up to all findings.")
	sample("k8sgpt_frontend_findings", `state="visible"`, float64(sum.Visible))
	sample("k8sgpt_frontend_findings", `state="hidden_rule"`, float64(sum.HiddenByRule))
	sample("k8sgpt_frontend_findings", `state="hidden_ignore"`, float64(sum.HiddenByIgnore))

	gauge("k8sgpt_frontend_visible_findings", "Visible findings by category, as grouped on the dashboard.")
	cats := make([]string, 0, len(sum.ByCategory))
	for c := range sum.ByCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		sample("k8sgpt_frontend_visible_findings", `category="`+escapeLabel(c)+`"`, float64(sum.ByCategory[c]))
	}

	gauge("k8sgpt_frontend_new_findings_24h", "Visible findings first seen in the last 24 hours.")
	sample("k8sgpt_frontend_new_findings_24h", "", float64(sum.New24h))

	gauge("k8sgpt_frontend_stale_ignores", "Hides of single findings that are no longer present.")
	sample("k8sgpt_frontend_stale_ignores", "", float64(len(resp.StaleIgnores)))

	gauge("k8sgpt_frontend_last_poll_timestamp_seconds", "Unix time results were last read successfully; 0 before the first.")
	var ts float64
	if last != nil {
		ts = float64(last.UnixMilli()) / 1000
	}
	sample("k8sgpt_frontend_last_poll_timestamp_seconds", "", ts)

	gauge("k8sgpt_frontend_last_poll_success", "1 if the latest poll read results and checked instances without error, else 0.")
	var success float64
	if ok {
		success = 1
	}
	sample("k8sgpt_frontend_last_poll_success", "", success)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b.Bytes())
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
