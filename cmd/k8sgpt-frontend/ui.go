package main

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
)

func registerHandlers(mux *http.ServeMux, clients *Clients, namespace string) {
	tmpl := template.Must(template.New("ui").Parse(uiTemplate))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/results", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		results, err := fetchResults(clients, namespace)
		if err != nil {
			log.Printf("api/results: %v", err)
			http.Error(w, "failed to fetch results", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		results, err := fetchResults(clients, namespace)
		if err != nil {
			log.Printf("ui: %v", err)
			http.Error(w, "failed to fetch results", http.StatusInternalServerError)
			return
		}

		type kindGroup struct {
			Kind  string
			Items []Result
		}
		type nsGroup struct {
			Namespace  string
			KindGroups []kindGroup
		}

		// Group results by namespace then by kind, preserving insertion order.
		nsMap := map[string]map[string][]Result{}
		nsOrder := []string{}
		for _, res := range results {
			ns := res.Namespace
			if ns == "" {
				ns = "(cluster-scoped)"
			}
			if _, ok := nsMap[ns]; !ok {
				nsMap[ns] = map[string][]Result{}
				nsOrder = append(nsOrder, ns)
			}
			nsMap[ns][res.Kind] = append(nsMap[ns][res.Kind], res)
		}

		groups := make([]nsGroup, 0, len(nsOrder))
		for _, ns := range nsOrder {
			kindMap := nsMap[ns]
			kgs := make([]kindGroup, 0, len(kindMap))
			for k, items := range kindMap {
				kgs = append(kgs, kindGroup{Kind: k, Items: items})
			}
			groups = append(groups, nsGroup{Namespace: ns, KindGroups: kgs})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, groups); err != nil {
			log.Printf("ui: template execute error: %v", err)
		}
	})
}

const uiTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>K8sGPT Results</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #0f1117;
    color: #e0e0e0;
    min-height: 100vh;
    padding: 2rem 1rem;
  }
  h1 {
    font-size: 1.6rem;
    font-weight: 700;
    margin-bottom: 2rem;
    color: #f0f0f0;
    letter-spacing: -0.02em;
  }
  h1 span { color: #6b8afd; }
  .empty {
    text-align: center;
    margin-top: 4rem;
    color: #555;
    font-size: 1rem;
  }
  .ns-section { margin-bottom: 2.5rem; }
  .ns-label {
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: #6b8afd;
    margin-bottom: 0.75rem;
  }
  .kind-section { margin-bottom: 1.5rem; }
  .kind-label {
    font-size: 0.75rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: #aaa;
    margin-bottom: 0.5rem;
    padding-left: 0.25rem;
  }
  .card {
    background: #1a1d27;
    border: 1px solid #2a2d3a;
    border-radius: 8px;
    padding: 1rem 1.25rem;
    margin-bottom: 0.75rem;
    transition: border-color 0.15s;
  }
  .card:hover { border-color: #6b8afd44; }
  .card-header {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    margin-bottom: 0.5rem;
  }
  .badge {
    display: inline-block;
    padding: 0.15rem 0.5rem;
    border-radius: 4px;
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    background: #2d2f45;
    color: #9ba8ff;
  }
  .card-name {
    font-size: 0.9rem;
    font-weight: 600;
    color: #dde;
  }
  .card-backend {
    margin-left: auto;
    font-size: 0.7rem;
    color: #555;
  }
  .error-list {
    margin: 0.5rem 0 0.75rem 0;
    padding: 0;
    list-style: none;
  }
  .error-list li {
    background: #251d1d;
    border-left: 3px solid #e05252;
    border-radius: 0 4px 4px 0;
    padding: 0.35rem 0.6rem;
    margin-bottom: 0.3rem;
    font-size: 0.8rem;
    color: #e09090;
    font-family: "SFMono-Regular", "Consolas", "Liberation Mono", monospace;
  }
  .card-details {
    font-size: 0.82rem;
    color: #99a;
    line-height: 1.55;
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
</head>
<body>
<h1><span>K8sGPT</span> Results</h1>
{{if not .}}
<p class="empty">No results found.</p>
{{else}}
{{range .}}
<div class="ns-section">
  <div class="ns-label">Namespace: {{.Namespace}}</div>
  {{range .KindGroups}}
  <div class="kind-section">
    <div class="kind-label">{{.Kind}}</div>
    {{range .Items}}
    <div class="card">
      <div class="card-header">
        <span class="badge">{{.Kind}}</span>
        <span class="card-name">{{.Name}}</span>
        {{if .Backend}}<span class="card-backend">via {{.Backend}}</span>{{end}}
      </div>
      {{if .Errors}}
      <ul class="error-list">
        {{range .Errors}}<li>{{.}}</li>{{end}}
      </ul>
      {{end}}
      {{if .Details}}<div class="card-details">{{.Details}}</div>{{end}}
    </div>
    {{end}}
  </div>
  {{end}}
</div>
{{end}}
{{end}}
</body>
</html>`
