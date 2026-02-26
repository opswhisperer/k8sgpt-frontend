package main

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Result is the normalised view of a K8sGPT Result CR.
type Result struct {
	UID       string   `json:"uid"`
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Kind      string   `json:"kind"`
	Details   string   `json:"details"`
	Errors    []string `json:"errors"`
	Backend   string   `json:"backend"`
}

// Clients groups the two clients needed throughout the app.
type Clients struct {
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
}

// buildClients constructs both the dynamic and discovery clients from the given config.
func buildClients(cfg *rest.Config) (*Clients, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{Dynamic: dyn, Discovery: disc}, nil
}

// findResultGVR uses server-side discovery to locate the GVR for resources
// whose name contains "result" in API groups containing "k8sgpt".
func findResultGVR(disc discovery.DiscoveryInterface) (schema.GroupVersionResource, error) {
	lists, err := disc.ServerPreferredResources()
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	for _, list := range lists {
		if !strings.Contains(list.GroupVersion, "k8sgpt") {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(strings.ToLower(r.Name), "result") {
				return schema.GroupVersionResource{
					Group:    gv.Group,
					Version:  gv.Version,
					Resource: r.Name,
				}, nil
			}
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("no k8sgpt result resource found")
}

// fetchResults lists all K8sGPT Result CRs from the given namespace and
// returns them as normalised Result values.
func fetchResults(clients *Clients, namespace string) ([]Result, error) {
	gvr, err := findResultGVR(clients.Discovery)
	if err != nil {
		return nil, err
	}

	list, err := clients.Dynamic.Resource(gvr).Namespace(namespace).List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(list.Items))
	for _, item := range list.Items {
		uid := string(item.GetUID())
		ns := item.GetNamespace()

		spec, _ := item.Object["spec"].(map[string]interface{})
		if spec == nil {
			spec = map[string]interface{}{}
		}

		details, _ := spec["details"].(string)
		kind, _ := spec["kind"].(string)
		name, _ := spec["name"].(string)
		backend, _ := spec["backend"].(string)

		var errors []string
		if rawErrors, ok := spec["error"].([]interface{}); ok {
			for _, e := range rawErrors {
				if em, ok := e.(map[string]interface{}); ok {
					if t, ok := em["text"].(string); ok {
						errors = append(errors, t)
					}
				}
			}
		}

		results = append(results, Result{
			UID:       uid,
			Name:      name,
			Namespace: ns,
			Kind:      kind,
			Details:   details,
			Errors:    errors,
			Backend:   backend,
		})
	}
	return results, nil
}
