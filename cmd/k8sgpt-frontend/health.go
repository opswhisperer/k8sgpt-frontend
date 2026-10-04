package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Health statuses, worst last.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusFailing  = "failing"
)

func statusRank(s string) int {
	switch s {
	case StatusFailing:
		return 2
	case StatusDegraded:
		return 1
	}
	return 0
}

// Health is the service health snapshot served at /api/health.
type Health struct {
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	CheckedAt time.Time      `json:"checked_at"`
	Instances []Instance     `json:"instances"`
	Frontend  FrontendHealth `json:"frontend"`
}

// Instance describes one K8sGPT CR: which AI backend and model it uses and
// whether its analysis is currently succeeding.
type Instance struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Backend    string   `json:"backend"`
	Model      string   `json:"model"`
	BaseURL    string   `json:"base_url,omitempty"`
	AIEnabled  bool     `json:"ai_enabled"`
	Anonymized bool     `json:"anonymized"`
	Version    string   `json:"version,omitempty"`
	Filters    []string `json:"filters,omitempty"`
	SecretName string   `json:"secret_name,omitempty"`
	SecretKey  string   `json:"secret_key,omitempty"`
	Server     Server   `json:"server"`
	Analysis   Analysis `json:"analysis"`
}

// Server is the k8sgpt server Deployment the operator runs for a K8sGPT CR.
type Server struct {
	Found   bool   `json:"found"`
	Ready   int64  `json:"ready"`
	Desired int64  `json:"desired"`
	Error   string `json:"error,omitempty"`
}

// Analysis is the most recent analysis failure, if any.
type Analysis struct {
	Status  string     `json:"status"` // ok, failing, disabled
	Class   string     `json:"class,omitempty"`
	Reason  string     `json:"reason,omitempty"`
	Hint    string     `json:"hint,omitempty"`
	Message string     `json:"message,omitempty"`
	Count   int64      `json:"count,omitempty"`
	FirstAt *time.Time `json:"first_at,omitempty"`
	LastAt  *time.Time `json:"last_at,omitempty"`
	Source  string     `json:"source,omitempty"` // "status" or "event"
}

// FrontendHealth is this app's own state.
type FrontendHealth struct {
	Version       string         `json:"version"`
	LastPoll      *time.Time     `json:"last_poll,omitempty"`
	LastPollError string         `json:"last_poll_error,omitempty"`
	PollInterval  string         `json:"poll_interval"`
	HealthWindow  string         `json:"health_window"`
	StoreWritable bool           `json:"store_writable"`
	ReadOnly      bool           `json:"read_only"`
	Notifier      NotifierStatus `json:"notifier"`
}

type errorClass struct {
	class, reason string
	re            *regexp.Regexp
}

// errorClasses are checked in order; the first match wins. Quota comes before
// rate limiting because providers report exhausted credit as HTTP 429 too.
var errorClasses = []errorClass{
	{"auth", "API key rejected", regexp.MustCompile(`(?i)status code: 40[13]\b|\b40[13] (unauthorized|forbidden)|incorrect api key|invalid[_ ]api[_ ]key|authentication|permission denied|unauthorized`)},
	{"quota", "usage quota or credit exhausted", regexp.MustCompile(`(?i)insufficient_quota|exceeded your current quota|quota exceeded|status code: 402\b|billing|credit balance`)},
	{"rate_limit", "rate limited", regexp.MustCompile(`(?i)status code: 429\b|\b429\b|rate.?limit|too many requests|overloaded`)},
	{"model", "model not found", regexp.MustCompile(`(?i)model_not_found|model .*does not exist|no such model|unknown model|status code: 404\b`)},
	{"unreachable", "backend unreachable", regexp.MustCompile(`(?i)connection refused|no such host|i/o timeout|deadline exceeded|context deadline|timeout|status code: 5\d\d\b|\bEOF\b|connection reset|\btls\b`)},
}

// classifyError maps an AI backend error message to a class and a short reason.
func classifyError(msg string) (class, reason string) {
	for _, c := range errorClasses {
		if c.re.MatchString(msg) {
			return c.class, c.reason
		}
	}
	return "other", "analysis failed"
}

func hintFor(class string, in *Instance) string {
	switch class {
	case "auth":
		if in.SecretName != "" {
			return fmt.Sprintf("Check the API key in Secret %q (key %q) and that it is valid for %s.", in.SecretName, in.SecretKey, in.Backend)
		}
		return "Check the API key configured for the AI backend."
	case "quota":
		return fmt.Sprintf("The %s account has run out of credit or quota; add credit or raise the limit.", in.Backend)
	case "rate_limit":
		return "The AI provider is throttling requests; this usually clears on its own. Consider fewer filters or a higher tier."
	case "model":
		return fmt.Sprintf("Model %q is not available on %s; check spec.ai.model on the K8sGPT resource.", in.Model, in.Backend)
	case "unreachable":
		return "The k8sgpt server cannot reach the AI backend; check network egress, baseUrl and the provider's status page."
	}
	return "See the k8sgpt server and operator logs for details."
}

var secretLike = regexp.MustCompile(`\b(sk-[A-Za-z0-9_\-*]{4})[A-Za-z0-9_\-*]{8,}`)

// scrubMessage shortens an error message and masks anything that looks like an API key.
func scrubMessage(msg string) string {
	msg = secretLike.ReplaceAllString(strings.TrimSpace(msg), "$1…")
	if len(msg) > 600 {
		msg = msg[:600] + "…"
	}
	return msg
}

func str(obj map[string]interface{}, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return s
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil && !t.IsZero() {
			return &t
		}
	}
	return nil
}

// eventTime returns the most recent time an Event was observed.
func eventTime(ev map[string]interface{}) *time.Time {
	for _, f := range [][]string{{"series", "lastObservedTime"}, {"lastTimestamp"}, {"eventTime"}, {"metadata", "creationTimestamp"}} {
		if t := parseTime(str(ev, f...)); t != nil {
			return t
		}
	}
	return nil
}

// buildInstance turns a K8sGPT CR, its warning events and server Deployment into an Instance.
func buildInstance(obj map[string]interface{}, events []unstructured.Unstructured, deploy *unstructured.Unstructured, deployErr error, window time.Duration, now time.Time) Instance {
	in := Instance{
		Name:       str(obj, "metadata", "name"),
		Backend:    str(obj, "spec", "ai", "backend"),
		Model:      str(obj, "spec", "ai", "model"),
		BaseURL:    str(obj, "spec", "ai", "baseUrl"),
		Version:    str(obj, "spec", "version"),
		SecretName: str(obj, "spec", "ai", "secret", "name"),
		SecretKey:  str(obj, "spec", "ai", "secret", "key"),
	}
	in.AIEnabled, _, _ = unstructured.NestedBool(obj, "spec", "ai", "enabled")
	in.Anonymized, _, _ = unstructured.NestedBool(obj, "spec", "ai", "anonymized")
	in.Filters, _, _ = unstructured.NestedStringSlice(obj, "spec", "filters")

	// Server deployment.
	switch {
	case deployErr != nil && apierrors.IsNotFound(deployErr):
		in.Server.Error = "not found"
	case deployErr != nil:
		in.Server.Error = deployErr.Error()
	case deploy != nil:
		in.Server.Found = true
		in.Server.Desired, _, _ = unstructured.NestedInt64(deploy.Object, "spec", "replicas")
		in.Server.Ready, _, _ = unstructured.NestedInt64(deploy.Object, "status", "readyReplicas")
	}

	// Latest analysis failure: status fields (newer operators) or Warning events.
	var a Analysis
	if msg := str(obj, "status", "lastAnalysisError"); msg != "" {
		a = Analysis{Message: msg, LastAt: parseTime(str(obj, "status", "lastAnalysisErrorTime")), Source: "status"}
	}
	for _, ev := range events {
		e := ev.Object
		if str(e, "involvedObject", "name") != in.Name || str(e, "type") != "Warning" {
			continue
		}
		t := eventTime(e)
		if t == nil || (a.LastAt != nil && !t.After(*a.LastAt)) {
			continue
		}
		count, _, _ := unstructured.NestedInt64(e, "count")
		a = Analysis{
			Message: str(e, "message"),
			Reason:  str(e, "reason"),
			Count:   count,
			FirstAt: parseTime(str(e, "firstTimestamp")),
			LastAt:  t,
			Source:  "event",
		}
	}

	switch {
	case !in.AIEnabled:
		in.Analysis = Analysis{Status: "disabled"}
	case a.Message != "" && a.LastAt != nil && now.Sub(*a.LastAt) <= window:
		a.Status = StatusFailing
		a.Class, a.Reason = classifyError(a.Message)
		a.Hint = hintFor(a.Class, &in)
		a.Message = scrubMessage(a.Message)
		in.Analysis = a
	default:
		in.Analysis = Analysis{Status: StatusOK}
		if a.Message != "" && a.LastAt != nil {
			// Keep the last (now old) failure for context.
			in.Analysis.LastAt = a.LastAt
			in.Analysis.Class, _ = classifyError(a.Message)
			in.Analysis.Message = scrubMessage(a.Message)
		}
	}

	in.Status = StatusOK
	if in.Server.Found && in.Server.Ready < in.Server.Desired {
		in.Status = StatusDegraded
	}
	if in.Server.Found && in.Server.Desired > 0 && in.Server.Ready == 0 {
		in.Status = StatusFailing
	}
	if in.Analysis.Status == StatusFailing {
		in.Status = StatusFailing
	}
	return in
}

// checkInstances reads every K8sGPT CR and builds its Instance.
func checkInstances(ctx context.Context, kube *Kube, window time.Duration, now time.Time) ([]Instance, error) {
	crs, err := kube.K8sGPTs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list K8sGPT resources: %w", err)
	}
	events, evErr := kube.K8sGPTEvents(ctx)
	instances := make([]Instance, 0, len(crs))
	for _, cr := range crs {
		deploy, derr := kube.Deployment(ctx, cr.GetName())
		in := buildInstance(cr.Object, events, deploy, derr, window, now)
		if evErr != nil && in.Analysis.Source == "" {
			in.Analysis.Message = "cannot read events: " + evErr.Error()
		}
		instances = append(instances, in)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })
	return instances, nil
}

// summarise sets the overall status and one-line summary.
func (h *Health) summarise(namespace string, instErr error) {
	h.Status = StatusOK
	var parts []string
	raise := func(s string) {
		if statusRank(s) > statusRank(h.Status) {
			h.Status = s
		}
	}
	if instErr != nil {
		raise(StatusDegraded)
		parts = append(parts, instErr.Error())
	} else if len(h.Instances) == 0 {
		raise(StatusDegraded)
		parts = append(parts, fmt.Sprintf("no K8sGPT resource found in namespace %s", namespace))
	}
	for _, in := range h.Instances {
		raise(in.Status)
		label := in.Name
		if in.Model != "" {
			label = fmt.Sprintf("%s (%s · %s)", in.Name, in.Backend, in.Model)
		}
		switch {
		case in.Analysis.Status == StatusFailing:
			parts = append(parts, fmt.Sprintf("%s: analysis failing — %s", label, in.Analysis.Reason))
		case in.Server.Found && in.Server.Ready < in.Server.Desired:
			parts = append(parts, fmt.Sprintf("%s: server %d/%d ready", label, in.Server.Ready, in.Server.Desired))
		}
	}
	if h.Frontend.LastPollError != "" {
		raise(StatusDegraded)
		parts = append(parts, "cannot read results: "+h.Frontend.LastPollError)
	}
	if h.Frontend.Notifier.LastError != "" {
		raise(StatusDegraded)
		parts = append(parts, "notifications failing: "+h.Frontend.Notifier.LastError)
	}
	if len(parts) == 0 {
		parts = append(parts, "all checks passing")
	}
	h.Summary = strings.Join(parts, "; ")
}
