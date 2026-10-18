# Example: Istio Gateway + cert-manager

Exposes k8sgpt-frontend over HTTPS through an Istio Gateway API `Gateway`, with a
cert-manager `Certificate`, and configures it from a local `settings.env`.

```bash
cp settings.env.example settings.env   # edit; settings.env is gitignored
kubectl apply -k deploy/examples/istio-gateway
```

To require a login, set the `OIDC_*` settings in `settings.env` (see the main README's
Login section) and put the client secret in a Secret:

```bash
kubectl -n k8sgpt-operator-system create secret generic k8sgpt-frontend-secrets \
  --from-literal=OIDC_CLIENT_SECRET=…
```

Without a login, anyone who can reach the hostname can hide findings; add
`READ_ONLY=true` to `settings.env` if that's not what you want.
