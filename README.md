# K8sGPT Frontend (Go)

Simple HTML frontend for K8sGPT operator results.

## Disclosure

This project was created and iterated with assistance from OpenAI Codex.

## Open-Source Attribution

Third-party dependency attribution is documented in:

- `THIRD_PARTY_NOTICES.md`

## What it does

- Reads K8sGPT result CRs from `k8sgpt-operator-system` (configurable).
- Works in-cluster (ServiceAccount token) and out-of-cluster (your kubeconfig).
- Renders a modern web UI grouped by:
  - Kubernetes namespace
  - Object type

## Run outside the cluster

Set kubeconfig or pass a flag:

```bash
go run ./cmd/k8sgpt-frontend --kubeconfig "$HOME/.kube/config" --result-namespace k8sgpt-operator-system
```

Then open [http://localhost:8080](http://localhost:8080).

Or using Make:

```bash
make build
./bin/k8sgpt-frontend --kubeconfig "$HOME/.kube/config" --result-namespace k8sgpt-operator-system
```

## Run inside the cluster

1. Build and push image:

```bash
docker build -t ghcr.io/<user>/k8sgpt-frontend:latest .
docker push ghcr.io/<user>/k8sgpt-frontend:latest
```

2. Configure Kustomize overlay values (kept out of git):

```bash
cp deploy/overlays/local/settings.env.example deploy/overlays/local/settings.env
# edit deploy/overlays/local/settings.env
```

3. Deploy:

```bash
make deploy
```

4. Access:

```bash
kubectl -n k8sgpt-operator-system port-forward svc/k8sgpt-frontend 8080:80
```

## Istio Gateway API ingress + cert-manager

Deployment is Kustomize-native:

- Base: `deploy/base`
- Local overlay: `deploy/overlays/local`
- Apply with: `kubectl apply -k deploy/overlays/local`

Overlay values come from `deploy/overlays/local/settings.env`:

- `INGRESS_HOSTNAME` for `Certificate`, `Gateway`, and `HTTPRoute`
- `IMAGE` for `Deployment` container image
- `CERT_ISSUER` for cert-manager issuer (default `letsencrypt-prod`)

Resources include:

- `Certificate` for your `INGRESS_HOSTNAME`
- `Gateway` (class `istio`) with HTTP/HTTPS listeners
- `HTTPRoute` for HTTP -> HTTPS redirect
- `HTTPRoute` for HTTPS traffic to `k8sgpt-frontend` service

## Config

Flags (or env vars):

- `--addr` / `ADDR` (default `:8080`)
- `--result-namespace` / `RESULT_NAMESPACE` (default `k8sgpt-operator-system`)
- `--kubeconfig` / `KUBECONFIG` (default `~/.kube/config`)

## API endpoint

- `GET /api/results` returns the raw normalized result list as JSON.

## Notes

- The app uses Kubernetes discovery to find `*result*` resources in groups containing `k8sgpt`, then lists results from the selected namespace.
- In-cluster auth is attempted first. If unavailable, kubeconfig is used.
