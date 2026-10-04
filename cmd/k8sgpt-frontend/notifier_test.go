package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestNotifyIssuesHTML(t *testing.T) {
	sink := newAppriseSink(t)
	n := &Notifier{URL: sink.URL, UIURL: "https://k8sgpt.example.com/", MaxItems: 15}
	r := res("Pod", "apps", "web", "container <script>alert(1)</script> crashed")
	r.Problem, r.Solution = "The pod crashes & restarts.", []string{"Check logs.", "Fix config."}
	if err := n.NotifyIssues([]Result{r}, false); err != nil {
		t.Fatal(err)
	}
	p := sink.all()[0]
	if p.Format != "html" || p.Type != "warning" || p.Title != "K8sGPT: Pod apps/web" {
		t.Errorf("payload meta = %+v", p)
	}
	for _, want := range []string{
		"<b>Namespace apps</b>",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"crashes &amp; restarts",
		"<ol><li>Check logs.</li><li>Fix config.</li></ol>",
		`href="https://k8sgpt.example.com/#q=Pod%2Fapps%2Fweb"`,
	} {
		if !strings.Contains(p.Body, want) {
			t.Errorf("body missing %q:\n%s", want, p.Body)
		}
	}
	if strings.Contains(p.Body, "<script>") {
		t.Error("body contains unescaped script tag")
	}
}

func TestNotifyIssuesCapsAndCompacts(t *testing.T) {
	sink := newAppriseSink(t)
	n := &Notifier{URL: sink.URL, MaxItems: 3}
	var rs []Result
	for i := 0; i < 8; i++ {
		r := res("Pod", "apps", fmt.Sprintf("web-%d", i), "crashed")
		r.Solution = []string{"restart"}
		rs = append(rs, r)
	}
	if err := n.NotifyIssues(rs, true); err != nil {
		t.Fatal(err)
	}
	p := sink.all()[0]
	if p.Title != "K8sGPT: 8 open issues" {
		t.Errorf("title = %q", p.Title)
	}
	if got := strings.Count(p.Body, "<code>web-"); got != 3 {
		t.Errorf("listed %d items, want 3", got)
	}
	if !strings.Contains(p.Body, "…and 5 more.") || strings.Contains(p.Body, "Suggested fix") {
		t.Errorf("expected compact body with overflow note:\n%s", p.Body)
	}
}

func TestNotifyFormats(t *testing.T) {
	for _, format := range []string{"markdown", "text"} {
		sink := newAppriseSink(t)
		n := &Notifier{URL: sink.URL, Format: format}
		r := res("Pod", "apps", "web", "crashed")
		r.Solution = []string{"a", "b"}
		if err := n.NotifyIssues([]Result{r}, false); err != nil {
			t.Fatal(err)
		}
		p := sink.all()[0]
		if p.Format != format || strings.Contains(p.Body, "<p>") || !strings.Contains(p.Body, "2. b") {
			t.Errorf("%s body:\n%s", format, p.Body)
		}
	}
}

func TestNotifyHealthAndErrors(t *testing.T) {
	sink := newAppriseSink(t)
	n := &Notifier{URL: sink.URL}
	h := Health{Summary: "k8sgpt: analysis failing — API key rejected", Instances: []Instance{{
		Name: "k8sgpt", Backend: "openai", Model: "gpt-5.4-mini",
		Analysis: Analysis{Status: StatusFailing, Reason: "API key rejected", Hint: "fix it", Message: "401"},
	}}}
	if err := n.NotifyHealth(h, false); err != nil {
		t.Fatal(err)
	}
	if err := n.NotifyHealth(h, true); err != nil {
		t.Fatal(err)
	}
	got := sink.all()
	if got[0].Type != "failure" || got[0].Title != "K8sGPT: openai — API key rejected" || !strings.Contains(got[0].Body, "gpt-5.4-mini") {
		t.Errorf("failure alert = %+v", got[0])
	}
	if got[1].Type != "success" || !strings.Contains(got[1].Body, "recovered") {
		t.Errorf("recovery = %+v", got[1])
	}

	sink.setStatus(http.StatusInternalServerError)
	if err := n.SendTest(nil); err == nil {
		t.Fatal("expected error from 500")
	}
	if st := n.Status(); st.LastError == "" || st.LastSent == nil || !st.Configured {
		t.Errorf("status after failure = %+v", st)
	}
}
