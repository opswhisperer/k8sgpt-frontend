package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// apprisePayload matches the Apprise API JSON body.
type apprisePayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Type  string `json:"type"`
}

// httpClient is reused across calls to benefit from TCP connection pooling.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// sendBatchNotification POSTs a single summary notification covering all
// provided results. One result produces an individual-style message; multiple
// results produce a numbered summary. Returns an error if the HTTP request
// fails or the server responds with a non-2xx status code.
func sendBatchNotification(url string, results []Result, uiURL string) error {
	if len(results) == 0 {
		return nil
	}

	var title, bodyText string

	if len(results) == 1 {
		r := results[0]
		title = fmt.Sprintf("K8sGPT: %s/%s (%s)", r.Namespace, r.Name, r.Kind)
		bodyText = fmt.Sprintf("Namespace: %s\nResource: %s/%s\n\n%s", r.Namespace, r.Kind, r.Name, r.Details)
	} else {
		title = fmt.Sprintf("K8sGPT: %d issues detected", len(results))
		var sections []string
		for i, r := range results {
			sections = append(sections, fmt.Sprintf(
				"[%d] %s/%s (%s)\nNamespace: %s\n\n%s",
				i+1, r.Kind, r.Name, r.Namespace, r.Namespace, r.Details,
			))
		}
		bodyText = strings.Join(sections, "\n\n---\n\n")
	}

	if uiURL != "" {
		bodyText += fmt.Sprintf("\n\nView in UI: %s", uiURL)
	}

	body, _ := json.Marshal(apprisePayload{
		Title: title,
		Body:  bodyText,
		Type:  "warning",
	})

	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("apprise POST failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("apprise returned status %d", resp.StatusCode)
	}
	return nil
}
