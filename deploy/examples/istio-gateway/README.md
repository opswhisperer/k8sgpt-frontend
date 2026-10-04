# Example: Istio Gateway + cert-manager

Exposes k8sgpt-frontend over HTTPS through an Istio Gateway API `Gateway`, with a
cert-manager `Certificate`, and configures it from a local `settings.env`.

```bash
cp settings.env.example settings.env   # edit; settings.env is gitignored
kubectl apply -k deploy/examples/istio-gateway
```

The dashboard has no login of its own. If the hostname is reachable by people who
should not hide findings, add `READ_ONLY=true` to `settings.env` or put an
authenticating proxy in front of it.
