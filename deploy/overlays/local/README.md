# Local overlay

1. Copy `settings.env.example` to `settings.env`.
2. Set `INGRESS_HOSTNAME`, `IMAGE`, and `CERT_ISSUER`.
3. Deploy with:

```bash
kubectl apply -k deploy/overlays/local
```
