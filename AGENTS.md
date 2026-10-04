# Notes for coding agents (and humans)

The README explains what k8sgpt-frontend is and how to run it. These are the rules that
aren't obvious from the code.

**Test:** `go vet ./... && go test ./...`. Tests use the client-go fake dynamic client and an
`httptest` Apprise sink; they never touch a real cluster or the network.

- **It never writes to the cluster.** RBAC is a namespaced, read-only Role
  (`deploy/base/rbac.yaml`) and the README promises it. POST endpoints may only change the
  app's own state (ignores, rules, refresh, test notification).
- **A new cluster read needs RBAC**: add the resource to `deploy/base/rbac.yaml`.
- **Rule matching exists twice**: `Rule.Match` in `cmd/k8sgpt-frontend/rules.go` and
  `matches`/`globRe` in `cmd/k8sgpt-frontend/web/index.html` (live match counts in the Hide
  dialog). Change both.
- **A finding's identity is its key** (`kind/namespace/name` from the Result's `spec`), not
  the Result CR's name or UID, so hides and notification state survive the operator
  recreating Results.
- **Persisted JSON (`ignores.json`, `rules.json`, `state.json`) must stay
  backward-compatible**: a deployed pod reads what the previous version wrote. Add fields;
  don't rename or repurpose them.
- **Notifications go through `html/template`** so finding text is escaped. Keep to tags
  Apprise converts for most services (`p b i code a ul ol li`).
- **Error messages can contain API keys.** Pass anything from the operator or the AI
  backend through `scrubMessage` before showing or sending it.
- **New settings** get a flag with an env fallback in `main.go`, a row in the README table,
  and (if they're commonly set) a line in `deploy/examples/istio-gateway/settings.env.example`.
  New rule fields go in `config/example.yaml`.
- **Dependencies stay minimal**: client-go, pflag and sigs.k8s.io/yaml. The UI is one HTML
  file with inline CSS/JS and no build step, embedded with `embed`.
- **Fixtures are invented.** Never paste in names, addresses or messages from a real cluster
  (beyond well-known components such as kube-system or Calico).
