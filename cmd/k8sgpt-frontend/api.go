package main

import (
	"embed"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

const (
	maxBody = 1 << 20
	maxKeys = 1000
)

// Summary counts findings for the dashboard tiles.
type Summary struct {
	Total      int            `json:"total"`
	Visible    int            `json:"visible"`
	Hidden     int            `json:"hidden"`
	New24h     int            `json:"new_24h"`
	ByCategory map[string]int `json:"by_category"`
}

type resultsResponse struct {
	Items        []Item     `json:"items"`
	Rules        []RuleView `json:"rules"`
	StaleIgnores []Ignore   `json:"stale_ignores"`
	Summary      Summary    `json:"summary"`
	Writable     bool       `json:"writable"`
	GeneratedAt  *time.Time `json:"generated_at,omitempty"`
}

func registerHandlers(mux *http.ServeMux, app *App) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		log.Fatalf("embedded UI missing: %v", err)
	}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !app.Ready() {
			http.Error(w, "waiting for first successful poll", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(index)
	})

	mux.HandleFunc("GET /api/results", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("raw") == "1" {
			results, _ := app.Snapshot()
			writeJSON(w, http.StatusOK, results)
			return
		}
		writeJSON(w, http.StatusOK, app.resultsResponse())
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, app.Health())
	})

	mux.HandleFunc("POST /api/refresh", app.post(false, func(w http.ResponseWriter, r *http.Request) {
		app.RequestRefresh()
		writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
	}))

	mux.HandleFunc("POST /api/ignore", app.post(true, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Keys  []string   `json:"keys"`
			Note  string     `json:"note"`
			Until *time.Time `json:"until"`
		}
		if !decode(w, r, &req) {
			return
		}
		if len(req.Keys) == 0 || len(req.Keys) > maxKeys {
			writeError(w, http.StatusBadRequest, "keys must contain 1 to 1000 entries")
			return
		}
		results, _ := app.Snapshot()
		byKey := make(map[string]Result, len(results))
		for _, res := range results {
			byKey[res.Key] = res
		}
		now := time.Now().UTC()
		var igs []Ignore
		missing := []string{}
		for _, k := range req.Keys {
			res, ok := byKey[k]
			if !ok {
				missing = append(missing, k)
				continue
			}
			igs = append(igs, Ignore{Key: k, Kind: res.Kind, Namespace: res.Namespace, Name: res.Name,
				Note: truncate(req.Note, maxNote), At: now, Until: req.Until})
		}
		if len(igs) > 0 {
			if err := app.store.AddIgnores(igs); err != nil {
				storeError(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ignored": len(igs), "missing": missing})
	}))

	mux.HandleFunc("POST /api/unignore", app.post(true, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Keys []string `json:"keys"`
		}
		if !decode(w, r, &req) {
			return
		}
		if len(req.Keys) == 0 || len(req.Keys) > maxKeys {
			writeError(w, http.StatusBadRequest, "keys must contain 1 to 1000 entries")
			return
		}
		n, err := app.store.RemoveIgnores(req.Keys)
		if err != nil {
			storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"unignored": n})
	}))

	mux.HandleFunc("POST /api/rules", app.post(true, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Rule  Rule       `json:"rule"`
			Note  string     `json:"note"`
			Until *time.Time `json:"until"`
		}
		if !decode(w, r, &req) {
			return
		}
		rule := Rule{Kind: req.Rule.Kind, Namespace: req.Rule.Namespace, NameRegex: req.Rule.NameRegex,
			ErrorRegex: req.Rule.ErrorRegex, Note: req.Note, Until: req.Until}
		if rule.Note == "" {
			rule.Note = req.Rule.Note
		}
		saved, err := app.store.AddRule(rule)
		if err != nil {
			if errors.Is(err, ErrReadOnly) {
				storeError(w, err)
			} else {
				writeError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]Rule{"rule": saved})
	}))

	mux.HandleFunc("POST /api/rules/delete", app.post(true, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &req) {
			return
		}
		if strings.HasPrefix(req.ID, "config-") {
			writeError(w, http.StatusBadRequest, "rules from the config file can only be removed there")
			return
		}
		ok, err := app.store.DeleteRule(req.ID)
		if err != nil {
			storeError(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "no such rule")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
	}))

	mux.HandleFunc("POST /api/test-notification", app.post(false, func(w http.ResponseWriter, r *http.Request) {
		if app.opts.ReadOnly {
			writeError(w, http.StatusForbidden, "read-only mode")
			return
		}
		if !app.notifier.Enabled() {
			writeError(w, http.StatusBadRequest, "no Apprise URL configured")
			return
		}
		items, _, _ := app.Items()
		var sample []Result
		for _, it := range items {
			if it.Hidden == nil {
				sample = append(sample, it.Result)
			}
		}
		if err := app.notifier.SendTest(sample); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
	}))
}

func (app *App) resultsResponse() resultsResponse {
	items, rules, stale := app.Items()
	_, polled := app.Snapshot()
	now := time.Now()
	sum := Summary{Total: len(items), ByCategory: map[string]int{}}
	for _, it := range items {
		if it.Hidden != nil {
			sum.Hidden++
			continue
		}
		sum.Visible++
		cat := it.Category
		if cat == "" {
			cat = it.BaseKind
		}
		sum.ByCategory[cat]++
		if it.FirstSeen != nil && now.Sub(*it.FirstSeen) < 24*time.Hour {
			sum.New24h++
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Key < stale[j].Key })
	if stale == nil {
		stale = []Ignore{}
	}
	return resultsResponse{
		Items: items, Rules: rules, StaleIgnores: stale, Summary: sum,
		Writable:    app.store.Writable() && !app.opts.ReadOnly,
		GeneratedAt: polled,
	}
}

// post wraps a mutating handler: JSON content type only (a simple CSRF
// defence, since browsers cannot send it cross-site without a preflight),
// a body size cap, and the read-only switch for store writes.
func (app *App) post(writes bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		if writes && app.opts.ReadOnly {
			writeError(w, http.StatusForbidden, "read-only mode")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrReadOnly) {
		writeError(w, http.StatusServiceUnavailable, "ignores and rules cannot be saved: "+err.Error())
		return
	}
	log.Printf("store: %v", err)
	writeError(w, http.StatusInternalServerError, "failed to save")
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
