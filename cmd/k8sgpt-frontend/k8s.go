package main

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Result is the normalised view of a K8sGPT Result CR.
//
// K8sGPT stores every Result in its own namespace; the object the finding is
// about lives in spec.name ("ns/name", or just "name" when cluster-scoped) and
// spec.kind ("Pod", or "Category/Kind" such as "Security/ServiceAccount").
type Result struct {
	Key          string     `json:"key"`
	UID          string     `json:"uid"`
	ResultName   string     `json:"result_name"`
	Name         string     `json:"name"`
	Namespace    string     `json:"namespace"`
	Kind         string     `json:"kind"`
	Category     string     `json:"category,omitempty"`
	BaseKind     string     `json:"base_kind"`
	ParentObject string     `json:"parent_object,omitempty"`
	Owner        string     `json:"owner,omitempty"` // name of the K8sGPT CR that wrote it
	Lifecycle    string     `json:"lifecycle,omitempty"`
	Backend      string     `json:"backend,omitempty"`
	Errors       []string   `json:"errors"`
	Details      string     `json:"details,omitempty"`
	Problem      string     `json:"problem,omitempty"`
	Solution     []string   `json:"solution,omitempty"`
	Created      *time.Time `json:"created,omitempty"`
	Updated      *time.Time `json:"updated,omitempty"`
}

// resultKey identifies a finding independently of the Result CR that carries
// it, so ignores and notification state survive the CR being recreated.
func resultKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

// defaultGroupVersion is used when discovery cannot find the K8sGPT API group.
var defaultGroupVersion = schema.GroupVersion{Group: "core.k8sgpt.ai", Version: "v1alpha1"}

var (
	eventsGVR      = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

// Kube reads K8sGPT objects from one namespace.
type Kube struct {
	dyn       dynamic.Interface
	disc      discovery.DiscoveryInterface // nil = always use defaultGroupVersion
	namespace string

	mu sync.Mutex
	gv *schema.GroupVersion
}

func newKube(cfg *rest.Config, namespace string) (*Kube, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Kube{dyn: dyn, disc: disc, namespace: namespace}, nil
}

// groupVersion finds the K8sGPT API group's preferred version once and caches it.
// A failed lookup is not cached, so a later call retries.
func (k *Kube) groupVersion() schema.GroupVersion {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.gv != nil {
		return *k.gv
	}
	if k.disc == nil {
		return defaultGroupVersion
	}
	groups, err := k.disc.ServerGroups()
	if err != nil {
		return defaultGroupVersion
	}
	for _, g := range groups.Groups {
		if strings.Contains(g.Name, "k8sgpt") && g.PreferredVersion.Version != "" {
			gv := schema.GroupVersion{Group: g.Name, Version: g.PreferredVersion.Version}
			k.gv = &gv
			return gv
		}
	}
	return defaultGroupVersion
}

func (k *Kube) gvr(resource string) schema.GroupVersionResource {
	return k.groupVersion().WithResource(resource)
}

// Results lists all K8sGPT Result CRs and normalises them.
func (k *Kube) Results(ctx context.Context) ([]Result, error) {
	list, err := k.dyn.Resource(k.gvr("results")).Namespace(k.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(list.Items))
	for i := range list.Items {
		results = append(results, normaliseResult(&list.Items[i]))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Key < results[j].Key })
	return results, nil
}

func normaliseResult(item *unstructured.Unstructured) Result {
	kind, _, _ := unstructured.NestedString(item.Object, "spec", "kind")
	specName, _, _ := unstructured.NestedString(item.Object, "spec", "name")
	details, _, _ := unstructured.NestedString(item.Object, "spec", "details")
	backend, _, _ := unstructured.NestedString(item.Object, "spec", "backend")
	parent, _, _ := unstructured.NestedString(item.Object, "spec", "parentObject")
	lifecycle, _, _ := unstructured.NestedString(item.Object, "status", "lifecycle")

	ns, name := "", specName
	if i := strings.Index(specName, "/"); i >= 0 {
		ns, name = specName[:i], specName[i+1:]
	}
	category, baseKind := "", kind
	if i := strings.LastIndex(kind, "/"); i >= 0 {
		category, baseKind = kind[:i], kind[i+1:]
	}

	errs := []string{}
	if raw, ok, _ := unstructured.NestedSlice(item.Object, "spec", "error"); ok {
		for _, e := range raw {
			if em, ok := e.(map[string]interface{}); ok {
				if t, ok := em["text"].(string); ok && t != "" {
					errs = append(errs, t)
				}
			}
		}
	}

	problem, solution := parseDetails(details)
	r := Result{
		Key:          resultKey(kind, ns, name),
		UID:          string(item.GetUID()),
		ResultName:   item.GetName(),
		Name:         name,
		Namespace:    ns,
		Kind:         kind,
		Category:     category,
		BaseKind:     baseKind,
		ParentObject: parent,
		Owner:        item.GetLabels()["k8sgpts.k8sgpt.ai/name"],
		Lifecycle:    lifecycle,
		Backend:      backend,
		Errors:       errs,
		Details:      details,
		Problem:      problem,
		Solution:     solution,
	}
	if ts := item.GetCreationTimestamp(); !ts.IsZero() {
		t := ts.Time
		r.Created = &t
	}
	var updated time.Time
	for _, mf := range item.GetManagedFields() {
		if mf.Time != nil && mf.Time.After(updated) {
			updated = mf.Time.Time
		}
	}
	if !updated.IsZero() {
		r.Updated = &updated
	}
	return r
}

var (
	solutionMarker = regexp.MustCompile(`(?i)\bsolution:\s*`)
	errorPrefix    = regexp.MustCompile(`(?i)^\s*error:\s*`)
	stepMarker     = regexp.MustCompile(`(?:^|\s)\d{1,2}[).]\s+`)
)

// parseDetails splits K8sGPT's AI explanation ("Error: …\n\nSolution: 1) … 2) …")
// into the problem statement and numbered solution steps. Text that does not
// follow that shape comes back as the problem with no steps.
func parseDetails(details string) (problem string, steps []string) {
	details = strings.TrimSpace(details)
	if details == "" {
		return "", nil
	}
	loc := solutionMarker.FindStringIndex(details)
	if loc == nil {
		return errorPrefix.ReplaceAllString(details, ""), nil
	}
	problem = strings.TrimSpace(errorPrefix.ReplaceAllString(details[:loc[0]], ""))
	solution := strings.TrimSpace(details[loc[1]:])

	idx := stepMarker.FindAllStringIndex(solution, -1)
	if len(idx) < 2 {
		if solution != "" {
			steps = []string{strings.TrimSpace(stepMarker.ReplaceAllString(solution, " "))}
		}
		return problem, steps
	}
	if lead := strings.TrimSpace(solution[:idx[0][0]]); lead != "" {
		steps = append(steps, lead)
	}
	for i, m := range idx {
		end := len(solution)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		if s := strings.TrimSpace(solution[m[1]:end]); s != "" {
			steps = append(steps, s)
		}
	}
	return problem, steps
}

// K8sGPTs lists the K8sGPT CRs (the analyser configuration) in the namespace.
func (k *Kube) K8sGPTs(ctx context.Context) ([]unstructured.Unstructured, error) {
	list, err := k.dyn.Resource(k.gvr("k8sgpts")).Namespace(k.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// K8sGPTEvents lists the Events recorded against K8sGPT CRs in the namespace.
// The operator records a Warning "AnalysisFailed" event when the AI backend
// call fails, which is the most reliable signal across operator versions.
func (k *Kube) K8sGPTEvents(ctx context.Context) ([]unstructured.Unstructured, error) {
	list, err := k.dyn.Resource(eventsGVR).Namespace(k.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []unstructured.Unstructured
	for _, ev := range list.Items {
		if kind, _, _ := unstructured.NestedString(ev.Object, "involvedObject", "kind"); kind == "K8sGPT" {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Deployment fetches a Deployment in the namespace; the operator names the
// k8sgpt server Deployment after its K8sGPT CR.
func (k *Kube) Deployment(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return k.dyn.Resource(deploymentsGVR).Namespace(k.namespace).Get(ctx, name, metav1.GetOptions{})
}
