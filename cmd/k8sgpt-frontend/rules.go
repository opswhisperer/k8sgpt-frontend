package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	maxRuleField = 200
	maxNote      = 500
)

// Ignore hides one finding, identified by its Result key.
type Ignore struct {
	Key       string     `json:"key"`
	Kind      string     `json:"kind"`
	Namespace string     `json:"namespace"`
	Name      string     `json:"name"`
	Note      string     `json:"note,omitempty"`
	At        time.Time  `json:"at"`
	Until     *time.Time `json:"until,omitempty"`
}

// Rule hides every finding it matches. Every field that is set must match:
//   - kind and namespace are case-insensitive globs ("Security/*", "*ServiceAccount",
//     "longhorn-*"); kind is tried against both the full kind and the part after "/".
//   - name_regex and error_regex are unanchored regular expressions; error_regex
//     matches if any of the finding's error lines matches.
type Rule struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind,omitempty"`
	Namespace  string     `json:"namespace,omitempty"`
	NameRegex  string     `json:"name_regex,omitempty"`
	ErrorRegex string     `json:"error_regex,omitempty"`
	Note       string     `json:"note,omitempty"`
	At         *time.Time `json:"at,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
	From       string     `json:"from,omitempty"` // "dashboard" or "config"

	nameRe, errRe *regexp.Regexp
}

// compile validates the rule and prepares its regular expressions.
func (r *Rule) compile() error {
	r.Kind = strings.TrimSpace(r.Kind)
	r.Namespace = strings.TrimSpace(r.Namespace)
	if r.Kind == "" && r.Namespace == "" && r.NameRegex == "" && r.ErrorRegex == "" {
		return errors.New("rule needs at least one of kind, namespace, name_regex, error_regex")
	}
	for field, v := range map[string]string{"kind": r.Kind, "namespace": r.Namespace, "name_regex": r.NameRegex, "error_regex": r.ErrorRegex} {
		if len(v) > maxRuleField {
			return fmt.Errorf("%s is longer than %d characters", field, maxRuleField)
		}
	}
	for field, v := range map[string]string{"kind": r.Kind, "namespace": r.Namespace} {
		if _, err := path.Match(strings.ToLower(v), ""); err != nil {
			return fmt.Errorf("%s is not a valid glob: %v", field, err)
		}
	}
	var err error
	r.nameRe, r.errRe = nil, nil
	if r.NameRegex != "" {
		if r.nameRe, err = regexp.Compile(r.NameRegex); err != nil {
			return fmt.Errorf("name_regex: %v", err)
		}
	}
	if r.ErrorRegex != "" {
		if r.errRe, err = regexp.Compile(r.ErrorRegex); err != nil {
			return fmt.Errorf("error_regex: %v", err)
		}
	}
	r.Note = truncate(r.Note, maxNote)
	return nil
}

func globMatch(pattern, s string) bool {
	ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(s))
	return ok
}

// Match reports whether the rule selects the finding. The rule must have been compiled.
func (r *Rule) Match(res *Result) bool {
	if r.Kind != "" && !globMatch(r.Kind, res.Kind) && !globMatch(r.Kind, res.BaseKind) {
		return false
	}
	if r.Namespace != "" && !globMatch(r.Namespace, res.Namespace) {
		return false
	}
	if r.nameRe != nil && !r.nameRe.MatchString(res.Name) {
		return false
	}
	if r.errRe != nil {
		hit := false
		for _, e := range res.Errors {
			if r.errRe.MatchString(e) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func expired(until *time.Time, now time.Time) bool {
	return until != nil && !now.Before(*until)
}

// Hidden explains why a finding is hidden.
type Hidden struct {
	By     string     `json:"by"` // "ignore" or "rule"
	RuleID string     `json:"rule_id,omitempty"`
	From   string     `json:"from,omitempty"`
	Note   string     `json:"note,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
}

// RuleView is a rule as reported by the API.
type RuleView struct {
	Rule
	Matches int  `json:"matches"`
	Expired bool `json:"expired,omitempty"`
}

// Item is a finding as reported by the API.
type Item struct {
	Result
	Hidden    *Hidden    `json:"hidden,omitempty"`
	FirstSeen *time.Time `json:"first_seen,omitempty"`
}

// applyRules decides which findings are hidden. Precedence: a single ignore,
// then dashboard rules, then config rules (in the order given). Every active
// rule's match count includes findings an earlier layer already hid, so the
// count shows what removing the rule would reveal at most.
func applyRules(results []Result, ignores map[string]Ignore, rules []Rule, now time.Time) ([]Item, []RuleView, []Ignore) {
	items := make([]Item, len(results))
	views := make([]RuleView, len(rules))
	for i := range rules {
		views[i] = RuleView{Rule: rules[i], Expired: expired(rules[i].Until, now)}
	}
	present := make(map[string]bool, len(results))
	for i := range results {
		res := &results[i]
		present[res.Key] = true
		items[i] = Item{Result: *res}
		if ig, ok := ignores[res.Key]; ok && !expired(ig.Until, now) {
			at := ig.At
			items[i].Hidden = &Hidden{By: "ignore", Note: ig.Note, At: &at, Until: ig.Until}
		}
		for j := range views {
			if views[j].Expired || !rules[j].Match(res) {
				continue
			}
			views[j].Matches++
			if items[i].Hidden == nil {
				r := &rules[j]
				items[i].Hidden = &Hidden{By: "rule", RuleID: r.ID, From: r.From, Note: r.Note, At: r.At, Until: r.Until}
			}
		}
	}
	var stale []Ignore
	for key, ig := range ignores {
		if !present[key] {
			stale = append(stale, ig)
		}
	}
	return items, views, stale
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
