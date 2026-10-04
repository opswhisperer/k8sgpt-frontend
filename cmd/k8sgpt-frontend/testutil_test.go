package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

const testNS = "k8sgpt-operator-system"

// The real 401 the operator reported in a live cluster.
const msg401 = "failed to call Analyze RPC: rpc error: code = Unknown desc = failed while calling AI provider openai: error, status code: 401, status: 401 Unauthorized, message: Incorrect API key provided: sk-proj-abcdefghijklmnopqrstuvwxyz. You can find your API key at https://platform.openai.com/account/api-keys."

func newFakeKube(t *testing.T, objs ...runtime.Object) (*Kube, *dynfake.FakeDynamicClient) {
	t.Helper()
	gv := defaultGroupVersion
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gv.WithResource("results"): "ResultList",
		gv.WithResource("k8sgpts"): "K8sGPTList",
		eventsGVR:                  "EventList",
		deploymentsGVR:             "DeploymentList",
	}, objs...)
	return &Kube{dyn: dyn, namespace: testNS}, dyn
}

func resultObj(crName, kind, specName string, errs ...string) *unstructured.Unstructured {
	var errList []interface{}
	for _, e := range errs {
		errList = append(errList, map[string]interface{}{"text": e})
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "core.k8sgpt.ai/v1alpha1",
		"kind":       "Result",
		"metadata": map[string]interface{}{
			"name": crName, "namespace": testNS, "uid": "uid-" + crName, "creationTimestamp": "2026-06-11T16:34:31Z",
			"labels": map[string]interface{}{"k8sgpts.k8sgpt.ai/name": "k8sgpt"},
		},
		"spec": map[string]interface{}{
			"kind":    kind,
			"name":    specName,
			"backend": "openai",
			"details": "Error: Something is wrong.\n\nSolution: 1) First step. 2) Second step.",
			"error":   errList,
		},
		"status": map[string]interface{}{"lifecycle": "historical"},
	}}
}

func k8sgptObj(name string, aiEnabled bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "core.k8sgpt.ai/v1alpha1",
		"kind":       "K8sGPT",
		"metadata":   map[string]interface{}{"name": name, "namespace": testNS},
		"spec": map[string]interface{}{
			"version": "v0.4.39",
			"filters": []interface{}{"Pod", "Service"},
			"ai": map[string]interface{}{
				"enabled": aiEnabled, "anonymized": true, "backend": "openai", "model": "gpt-5.4-mini",
				"secret": map[string]interface{}{"name": "k8sgpt-sample-secret", "key": "openai-api-key"},
			},
		},
	}}
}

func eventObj(name, involved, msg string, last time.Time, count int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion":     "v1",
		"kind":           "Event",
		"metadata":       map[string]interface{}{"name": name, "namespace": testNS},
		"involvedObject": map[string]interface{}{"kind": "K8sGPT", "name": involved},
		"type":           "Warning",
		"reason":         "AnalysisFailed",
		"message":        msg,
		"count":          count,
		"firstTimestamp": last.Add(-24 * time.Hour).Format(time.RFC3339),
		"lastTimestamp":  last.Format(time.RFC3339),
	}}
}

func deploymentObj(name string, desired, ready int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": name, "namespace": testNS},
		"spec":       map[string]interface{}{"replicas": desired},
		"status":     map[string]interface{}{"readyReplicas": ready},
	}}
}

// appriseSink records every payload posted to it.
type appriseSink struct {
	*httptest.Server
	mu       sync.Mutex
	payloads []apprisePayload
	status   int
	reply    string
}

func newAppriseSink(t *testing.T) *appriseSink {
	s := &appriseSink{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p apprisePayload
		_ = json.Unmarshal(b, &p)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.payloads = append(s.payloads, p)
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.reply))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *appriseSink) all() []apprisePayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]apprisePayload(nil), s.payloads...)
}

func (s *appriseSink) setStatus(code int) { s.setReply(code, "") }

func (s *appriseSink) setReply(code int, body string) {
	s.mu.Lock()
	s.status, s.reply = code, body
	s.mu.Unlock()
}

// Apprise API's 424 body when one of three targets fails.
const partial424 = `{"error":"One or more notification could not be sent","details":[["INFO","2026-10-04 01:49:27,343","Notifying 3 service(s)."],["INFO","2026-10-04 01:49:27,551","Sent Pushover notification to ALL_DEVICES."],["WARNING","2026-10-04 01:49:27,605","Failed to send Discord notification: Bad Request - Unsupported Parameters., error=400."],["INFO","2026-10-04 01:49:27,634","Sent AWS SES notification."]]}`
