# k8sgpt-frontend

A dashboard and notifier for [K8sGPT](https://k8sgpt.ai) operator results.

The [K8sGPT operator](https://github.com/k8sgpt-ai/k8sgpt-operator) scans the cluster
and writes each problem it finds as a `Result` resource, with an AI-written
explanation and fix. k8sgpt-frontend turns those resources into something you can
live with:

- **A findings dashboard.** Each finding is grouped by the namespace it is really in,
  shows the analyser's error lines and the AI's suggested fix as steps, and links to
  `kubectl describe`.
- **Hide rules.** Hide one finding, or a whole group by kind, namespace, name pattern
  or error text, for good or for a set time. Hidden findings drop out of the list and
  never notify. Rules can be added from the dashboard or kept in git.
- **Notifications** through [Apprise](https://github.com/caronc/apprise-api) (Slack,
  Discord, Telegram, ntfy, email, …) as formatted HTML. A finding is notified once,
  after it has persisted for a while. Many findings arrive as one batched message.
- **K8sGPT health.** It shows which AI backend and model K8sGPT uses and whether
  analysis is working. If the API key is rejected, the account is out of credit, the
  provider is rate limiting, the model doesn't exist or the backend is unreachable, the
  dashboard says so, gives a fix hint, and sends one alert and one recovery notice.

It runs next to the operator and is **read-only** against the cluster.

## Quick start

You need the K8sGPT operator installed with a `K8sGPT` resource. The examples assume
the operator's default namespace, `k8sgpt-operator-system`.

```bash
kubectl apply -k 'https://github.com/opswhisperer/k8sgpt-frontend//deploy/base?ref=main'
kubectl -n k8sgpt-operator-system port-forward svc/k8sgpt-frontend 8080:80
```

Open <http://localhost:8080>. Images are published for `linux/amd64` and
`linux/arm64`. The base keeps hide rules and notification state on a 1Gi PVC, so you
need a default StorageClass.

## Configure

Settings are environment variables, which the base reads from an optional ConfigMap
`k8sgpt-frontend-settings` and an optional Secret `k8sgpt-frontend-secrets` (use the
Secret for an `APPRISE_URL` that contains credentials). Every setting also has a
command-line flag (`--apprise-url`, …); a flag beats its environment variable.
Defaults shown are the ones `deploy/base` sets.

| Variable | Default | |
|---|---|---|
| `APPRISE_URL` | *(off)* | Apprise API notify endpoint, e.g. `http://apprise.apps.svc:8000/notify/apprise` |
| `UI_URL` | | External URL of the dashboard, used for links in notifications |
| `APPRISE_FORMAT` | `html` | `html`, `markdown` or `text` |
| `APPRISE_TAG` | | Only notify Apprise URLs with this tag |
| `NOTIFY_DELAY` | `300` | Seconds a finding must persist before it is notified (`0` = at once) |
| `NOTIFY_MAX_ITEMS` | `15` | Most findings listed in one message (the rest are counted) |
| `NOTIFY_RESOLVED` | `false` | Also send one message when notified findings clear |
| `POLL_INTERVAL` | `60` | Seconds between polls |
| `HEALTH_WINDOW` | `2700` | Seconds an analysis failure counts as current (the operator retries every ~15 min) |
| `READ_ONLY` | `false` | Disable hiding, rule changes and test notifications in the dashboard |
| `RESULT_NAMESPACE` | pod namespace | Namespace of the `K8sGPT` and `Result` resources |
| `DATA_DIR` | `/data` | Where dashboard rules, hides and notification state are stored |
| `CONFIG` | `/config/config.yaml` | Hide rules kept in git (see below) |

A minimal overlay:

```yaml
# my-k8sgpt-frontend/kustomization.yaml
resources:
  - https://github.com/opswhisperer/k8sgpt-frontend//deploy/base?ref=main
configMapGenerator:
  - name: k8sgpt-frontend-settings
    literals:
      - APPRISE_URL=http://apprise.apps.svc.cluster.local:8000/notify/apprise
      - UI_URL=https://k8sgpt.example.com
  - name: k8sgpt-frontend-config   # replace the base's empty rule list
    behavior: replace
    files:
      - config.yaml
```

[`deploy/examples/istio-gateway`](deploy/examples/istio-gateway) adds an HTTPS
Gateway, HTTPRoutes and a cert-manager Certificate, with settings from a local
`settings.env`.

## Hiding findings

| You want to… | Do this |
|---|---|
| Stop seeing one finding | **Hide…** on its card → *Only this finding* |
| Hide it for a while | Pick *1 / 7 / 30 days* in the Hide dialog; it comes back afterwards |
| Hide everything like it | Pick a suggested rule (same kind here, same kind everywhere, this namespace, the whole category, the same error). Each one shows how many findings it would hide now |
| Write your own pattern | *Custom rule* in the Hide dialog (globs for kind and namespace, regex for name and error), or add it to `config.yaml` |
| See what's hidden, or undo | Tick **Show hidden**, or open **Rules** |

A rule matches when every field it sets matches:

- **`kind`** and **`namespace`** are case-insensitive globs. `kind` is tried against
  both the full K8sGPT kind (`Security/ServiceAccount`) and the part after the `/`
  (`ServiceAccount`), so `Security/*` hides a whole category.
- **`name_regex`** and **`error_regex`** are unanchored regular expressions.

[`config/example.yaml`](config/example.yaml) explains every field with examples.

## Health

The **health pill** in the header opens a panel showing:

- each `K8sGPT` resource's backend, model and version,
- whether AI analysis is enabled and anonymised,
- the k8sgpt server's readiness,
- the latest analysis failure, with a fix hint,
- this dashboard's own poll and notification status.

Analysis failures come from the operator's `AnalysisFailed` events (and from
`status.lastAnalysisError` on newer operators). They are classified as:

| Class | Typical cause |
|---|---|
| API key rejected | 401/403, invalid or revoked key |
| Quota or credit exhausted | `insufficient_quota`, billing limits |
| Rate limited | 429 without a quota message |
| Model not found | Wrong `spec.ai.model` for the backend |
| Backend unreachable | DNS, timeouts, connection refused, 5xx |

When analysis starts failing, one Apprise **failure** alert is sent. One **success**
notice follows when it recovers. Neither repeats, including across restarts.

## Notifications

- A finding is notified once it has been present for `NOTIFY_DELAY`. Findings that
  clear sooner are never sent.
- Findings that come due together are sent as one message, grouped by namespace.
- Up to five findings, the message includes the AI explanation and fix steps. Above
  that it lists only the error lines, to stay readable.
- A finding that clears and later comes back is notified again.
- On the very first start, the findings already present are sent as one "already
  present" summary.
- Use **Send test notification** in the health panel to check how messages look on
  your Apprise targets.

## Security

- **Cluster access** is read-only: a namespaced Role with `get/list/watch` on K8sGPT
  results and K8sGPT resources, and `get/list` on events and deployments.
- **The dashboard has no login.** Anyone who can reach it can hide findings. Set
  `READ_ONLY=true`, or put it behind an authenticating proxy, when exposing it beyond
  people who should do that.
- **Write endpoints** only accept `Content-Type: application/json`. Browsers cannot
  send that cross-site without a CORS preflight, which the server never allows.
- **Secrets in errors:** anything that looks like an API key in an error message is
  masked before it is shown or sent.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/results` | Findings with hide state, rules with match counts, summary |
| GET | `/api/results?raw=1` | Plain list of findings (the pre-1.0 format) |
| GET | `/api/health` | Health snapshot |
| POST | `/api/ignore` | `{"keys": [...], "note": "", "until": "RFC3339"}` |
| POST | `/api/unignore` | `{"keys": [...]}` |
| POST | `/api/rules` | `{"rule": {"kind", "namespace", "name_regex", "error_regex"}, "note", "until"}` |
| POST | `/api/rules/delete` | `{"id": "..."}` |
| POST | `/api/refresh` | Poll now |
| POST | `/api/test-notification` | Send a sample notification |
| GET | `/healthz`, `/readyz` | Liveness, and readiness after the first successful poll |

A finding's key is `kind/namespace/name`, for example
`Security/ServiceAccount/apps/default`. Cluster-scoped objects have an empty
namespace.

## Run outside the cluster

```bash
go run ./cmd/k8sgpt-frontend --kubeconfig ~/.kube/config --data-dir ./data
```

Then open <http://localhost:8080>. Add `--apprise-url http://localhost:8000/notify/`
to try notifications against a local
[Apprise API](https://github.com/caronc/apprise-api), for example
`docker run -p 8000:8000 -e APPRISE_STATELESS_URLS=… caronc/apprise-api`.

## Development

```bash
go vet ./... && go test ./...
./build.sh            # tests, then a multi-arch image push (IMAGE=… to override)
```

Pushing a `v*` tag makes CI publish `ghcr.io/<owner>/k8sgpt-frontend:<version>` and
`:latest`.

### Upgrading from 0.x

Version 0.x put everything in one manifest with the selector `app: k8sgpt-frontend`.
A Deployment's selector cannot change, so delete the old Deployment once before
applying the new base:

```bash
kubectl -n k8sgpt-operator-system delete deployment k8sgpt-frontend
```

The old un-hashed `k8sgpt-frontend-settings` ConfigMap is no longer used by the
example overlay and can be deleted.

## Disclosure

This project was created and iterated with assistance from OpenAI Codex & Claude Code.

## License

Apache 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). Third-party dependencies
are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
