package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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

// sendNotification POSTs a single notification to the Apprise API URL.
// It returns an error if the HTTP request fails or the server responds
// with a non-2xx status code.
func sendNotification(url string, r Result, uiURL string) error {
	bodyText := fmt.Sprintf("Namespace: %s\nResource: %s/%s\n\n%s", r.Namespace, r.Kind, r.Name, r.Details)
	if uiURL != "" {
		bodyText += fmt.Sprintf("\n\nView in UI: %s", uiURL)
	}
	body, _ := json.Marshal(apprisePayload{
		Title: fmt.Sprintf("K8sGPT: %s/%s (%s)", r.Namespace, r.Name, r.Kind),
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
