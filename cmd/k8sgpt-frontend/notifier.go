package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"
)

// apprisePayload matches the Apprise API /notify JSON body.
type apprisePayload struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Type   string `json:"type"`             // info, success, warning, failure
	Format string `json:"format,omitempty"` // html, markdown, text
	Tag    string `json:"tag,omitempty"`
}

// NotifierStatus is reported in /api/health.
type NotifierStatus struct {
	Configured  bool       `json:"configured"`
	Format      string     `json:"format,omitempty"`
	Tag         string     `json:"tag,omitempty"`
	LastSent    *time.Time `json:"last_sent,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

// Notifier sends messages to an Apprise API server.
type Notifier struct {
	URL      string
	UIURL    string
	Format   string // html (default), markdown or text
	Tag      string
	MaxItems int
	Client   *http.Client

	mu     sync.Mutex
	status NotifierStatus
}

func (n *Notifier) Enabled() bool { return n != nil && n.URL != "" }

func (n *Notifier) Status() NotifierStatus {
	if n == nil {
		return NotifierStatus{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.status
	s.Configured, s.Format, s.Tag = n.URL != "", n.format(), n.Tag
	return s
}

func (n *Notifier) format() string {
	switch n.Format {
	case "markdown", "text":
		return n.Format
	}
	return "html"
}

func (n *Notifier) send(title, body, typ string) error {
	err := n.post(apprisePayload{Title: title, Body: body, Type: typ, Format: n.format(), Tag: n.Tag})
	now := time.Now().UTC()
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		n.status.LastError, n.status.LastErrorAt = err.Error(), &now
		return err
	}
	n.status.LastSent, n.status.LastError, n.status.LastErrorAt = &now, "", nil
	return nil
}

func (n *Notifier) post(p apprisePayload) error {
	body, _ := json.Marshal(p)
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Post(n.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("apprise POST failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("apprise returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ---- issue messages ----

type issueView struct {
	Kind, Name, Link string
	Errors           []string
	Problem          string
	Solution         []string
}

type issueGroup struct {
	Label string
	Items []issueView
}

type issuesMsg struct {
	Intro    string
	Groups   []issueGroup
	More     int
	UIURL    string
	Resolved bool
}

func (n *Notifier) itemLink(key string) string {
	if n.UIURL == "" {
		return ""
	}
	return strings.TrimRight(n.UIURL, "/") + "/#q=" + url.QueryEscape(key)
}

// buildIssues groups results by namespace, keeping at most MaxItems.
// compactAbove is the batch size above which messages list only the error
// lines, not the AI explanation, to keep them readable.
const compactAbove = 5

func (n *Notifier) buildIssues(results []Result, intro string, resolved bool) issuesMsg {
	compact := len(results) > compactAbove
	sorted := append([]Result(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Key < sorted[j].Key
	})
	max := n.MaxItems
	if max <= 0 {
		max = 15
	}
	msg := issuesMsg{Intro: intro, UIURL: n.UIURL, Resolved: resolved}
	if len(sorted) > max {
		msg.More = len(sorted) - max
		sorted = sorted[:max]
	}
	for _, r := range sorted {
		label := "Cluster-scoped"
		if r.Namespace != "" {
			label = "Namespace " + r.Namespace
		}
		if len(msg.Groups) == 0 || msg.Groups[len(msg.Groups)-1].Label != label {
			msg.Groups = append(msg.Groups, issueGroup{Label: label})
		}
		v := issueView{Kind: r.Kind, Name: r.Name, Link: n.itemLink(r.Key)}
		if !resolved {
			v.Errors = r.Errors
			if !compact {
				v.Problem, v.Solution = r.Problem, r.Solution
				if v.Problem == "" && len(v.Solution) == 0 {
					v.Problem = r.Details
				}
			}
		}
		g := &msg.Groups[len(msg.Groups)-1]
		g.Items = append(g.Items, v)
	}
	return msg
}

var issuesHTML = htmltemplate.Must(htmltemplate.New("issues").Parse(`
{{- if .Intro}}<p>{{.Intro}}</p>{{end}}
{{- range .Groups}}
<p><b>{{.Label}}</b></p>
{{- range .Items}}
<p><b>{{.Kind}}</b> <code>{{.Name}}</code>{{if .Link}} · <a href="{{.Link}}">open</a>{{end}}</p>
{{- if .Errors}}
<ul>{{range .Errors}}<li>{{.}}</li>{{end}}</ul>
{{- end}}
{{- if .Problem}}
<p>{{.Problem}}</p>
{{- end}}
{{- if .Solution}}
<p><i>Suggested fix:</i></p>
<ol>{{range .Solution}}<li>{{.}}</li>{{end}}</ol>
{{- end}}
{{- end}}
{{- end}}
{{- if .More}}
<p>…and {{.More}} more.</p>
{{- end}}
{{- if .UIURL}}
<p><a href="{{.UIURL}}">Open the K8sGPT dashboard</a></p>
{{- end}}
`))

var textFuncs = texttemplate.FuncMap{"inc": func(i int) int { return i + 1 }}

var issuesMarkdown = texttemplate.Must(texttemplate.New("issues").Funcs(textFuncs).Parse(`
{{- if .Intro}}{{.Intro}}

{{end}}
{{- range .Groups}}### {{.Label}}
{{range .Items}}
**{{.Kind}}** ` + "`{{.Name}}`" + `{{if .Link}} · [open]({{.Link}}){{end}}
{{range .Errors}}- {{.}}
{{end}}
{{- if .Problem}}
{{.Problem}}
{{end}}
{{- if .Solution}}
_Suggested fix:_
{{range $i, $s := .Solution}}{{inc $i}}. {{$s}}
{{end}}{{end}}
{{- end}}
{{end}}
{{- if .More}}…and {{.More}} more.
{{end}}
{{- if .UIURL}}
[Open the K8sGPT dashboard]({{.UIURL}})
{{end}}`))

var issuesText = texttemplate.Must(texttemplate.New("issues").Funcs(textFuncs).Parse(`
{{- if .Intro}}{{.Intro}}

{{end}}
{{- range .Groups}}== {{.Label}} ==
{{range .Items}}
{{.Kind}} {{.Name}}
{{range .Errors}}  - {{.}}
{{end}}
{{- if .Problem}}  {{.Problem}}
{{end}}
{{- if .Solution}}  Suggested fix:
{{range $i, $s := .Solution}}    {{inc $i}}. {{$s}}
{{end}}{{end}}
{{- if .Link}}  {{.Link}}
{{end}}
{{- end}}
{{end}}
{{- if .More}}…and {{.More}} more.
{{end}}
{{- if .UIURL}}
View all: {{.UIURL}}
{{end}}`))

// render executes the template for the configured format.
func (n *Notifier) render(html *htmltemplate.Template, md, text *texttemplate.Template, data interface{}) (string, error) {
	var buf bytes.Buffer
	var err error
	switch n.format() {
	case "markdown":
		err = md.Execute(&buf, data)
	case "text":
		err = text.Execute(&buf, data)
	default:
		err = html.Execute(&buf, data)
	}
	return strings.TrimSpace(buf.String()), err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// NotifyIssues sends one message covering newly detected findings. When
// existing is true the findings were already present at first start.
func (n *Notifier) NotifyIssues(results []Result, existing bool) error {
	if len(results) == 0 {
		return nil
	}
	var title, intro string
	switch {
	case existing:
		title = fmt.Sprintf("K8sGPT: %d open %s", len(results), plural(len(results), "issue", "issues"))
		intro = "Monitoring started. These issues were already present:"
	case len(results) == 1:
		r := results[0]
		title = fmt.Sprintf("K8sGPT: %s %s", r.Kind, displayName(r))
	default:
		title = fmt.Sprintf("K8sGPT: %d new issues", len(results))
	}
	body, err := n.render(issuesHTML, issuesMarkdown, issuesText, n.buildIssues(results, intro, false))
	if err != nil {
		return err
	}
	return n.send(title, body, "warning")
}

// NotifyResolved sends one message listing findings that have cleared.
func (n *Notifier) NotifyResolved(results []Result) error {
	if len(results) == 0 {
		return nil
	}
	title := fmt.Sprintf("K8sGPT: %d %s resolved", len(results), plural(len(results), "issue", "issues"))
	body, err := n.render(issuesHTML, issuesMarkdown, issuesText, n.buildIssues(results, "No longer reported:", true))
	if err != nil {
		return err
	}
	return n.send(title, body, "success")
}

func displayName(r Result) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// ---- health messages ----

type healthMsg struct {
	Recovered bool
	Summary   string
	Instances []Instance
	UIURL     string
}

var healthFuncs = map[string]interface{}{
	"ago": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04 MST")
	},
}

var healthHTML = htmltemplate.Must(htmltemplate.New("health").Funcs(healthFuncs).Parse(`
{{- if .Recovered}}<p>K8sGPT analysis has recovered: {{.Summary}}.</p>
{{- else}}<p><b>{{.Summary}}</b></p>{{end}}
{{- range .Instances}}
<p><b>{{.Name}}</b>: {{.Backend}}{{if .Model}} · {{.Model}}{{end}}</p>
{{- if eq .Analysis.Status "failing"}}
<ul>
<li>Problem: {{.Analysis.Reason}}</li>
{{- if .Analysis.LastAt}}<li>Last failure: {{ago .Analysis.LastAt}}{{if gt .Analysis.Count 1}} ({{.Analysis.Count}} times){{end}}</li>{{end}}
<li>Fix: {{.Analysis.Hint}}</li>
</ul>
<p><code>{{.Analysis.Message}}</code></p>
{{- end}}
{{- if and .Server.Found (lt .Server.Ready .Server.Desired)}}
<p>k8sgpt server: {{.Server.Ready}}/{{.Server.Desired}} replicas ready</p>
{{- end}}
{{- end}}
{{- if .UIURL}}
<p><a href="{{.UIURL}}">Open the K8sGPT dashboard</a></p>
{{- end}}
`))

var healthMarkdown = texttemplate.Must(texttemplate.New("health").Funcs(healthFuncs).Parse(`
{{- if .Recovered}}K8sGPT analysis has recovered: {{.Summary}}.
{{else}}**{{.Summary}}**
{{end}}
{{- range .Instances}}
**{{.Name}}**: {{.Backend}}{{if .Model}} · {{.Model}}{{end}}
{{- if eq .Analysis.Status "failing"}}
- Problem: {{.Analysis.Reason}}
{{- if .Analysis.LastAt}}
- Last failure: {{ago .Analysis.LastAt}}{{if gt .Analysis.Count 1}} ({{.Analysis.Count}} times){{end}}{{end}}
- Fix: {{.Analysis.Hint}}

` + "`{{.Analysis.Message}}`" + `
{{- end}}
{{- if and .Server.Found (lt .Server.Ready .Server.Desired)}}
k8sgpt server: {{.Server.Ready}}/{{.Server.Desired}} replicas ready
{{- end}}
{{end}}
{{- if .UIURL}}
[Open the K8sGPT dashboard]({{.UIURL}})
{{end}}`))

var healthText = texttemplate.Must(texttemplate.New("health").Funcs(healthFuncs).Parse(`
{{- if .Recovered}}K8sGPT analysis has recovered: {{.Summary}}.
{{else}}{{.Summary}}
{{end}}
{{- range .Instances}}
{{.Name}}: {{.Backend}}{{if .Model}} · {{.Model}}{{end}}
{{- if eq .Analysis.Status "failing"}}
  Problem: {{.Analysis.Reason}}
{{- if .Analysis.LastAt}}
  Last failure: {{ago .Analysis.LastAt}}{{if gt .Analysis.Count 1}} ({{.Analysis.Count}} times){{end}}{{end}}
  Fix: {{.Analysis.Hint}}
  {{.Analysis.Message}}
{{- end}}
{{- if and .Server.Found (lt .Server.Ready .Server.Desired)}}
  k8sgpt server: {{.Server.Ready}}/{{.Server.Desired}} replicas ready
{{- end}}
{{end}}
{{- if .UIURL}}
{{.UIURL}}
{{end}}`))

// NotifyHealth sends a failure alert, or a recovery notice when recovered is true.
func (n *Notifier) NotifyHealth(h Health, recovered bool) error {
	title, typ := "K8sGPT: analysis failing", "failure"
	if recovered {
		title, typ = "K8sGPT: analysis recovered", "success"
	} else {
		for _, in := range h.Instances {
			if in.Analysis.Status == StatusFailing {
				title = fmt.Sprintf("K8sGPT: %s — %s", in.Backend, in.Analysis.Reason)
				break
			}
		}
	}
	body, err := n.render(healthHTML, healthMarkdown, healthText, healthMsg{Recovered: recovered, Summary: h.Summary, Instances: h.Instances, UIURL: n.UIURL})
	if err != nil {
		return err
	}
	return n.send(title, body, typ)
}

// SendTest sends a sample message built from real findings (or a placeholder).
func (n *Notifier) SendTest(sample []Result) error {
	if len(sample) == 0 {
		sample = []Result{{
			Key: "Pod/demo/example", Kind: "Pod", Namespace: "demo", Name: "example",
			Errors:   []string{"Back-off restarting failed container"},
			Problem:  "This is a test notification from k8sgpt-frontend.",
			Solution: []string{"Nothing to do — formatting check only."},
		}}
	}
	if len(sample) > 2 {
		sample = sample[:2]
	}
	body, err := n.render(issuesHTML, issuesMarkdown, issuesText, n.buildIssues(sample, "Test notification — this is how issues will look:", false))
	if err != nil {
		return err
	}
	return n.send("K8sGPT: test notification", body, "info")
}
