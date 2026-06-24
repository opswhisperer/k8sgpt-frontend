#!/usr/bin/env bash
set -euo pipefail

OVERLAY="${OVERLAY:-deploy/overlays/local}"
SETTINGS="$OVERLAY/settings.env"

if [[ ! -f "$SETTINGS" ]]; then
  echo "error: $SETTINGS not found (copy from settings.env.example)" >&2
  exit 1
fi

# Read IMAGE from settings.env, allow env-var override.
IMAGE="${IMAGE:-$(grep '^IMAGE=' "$SETTINGS" | cut -d= -f2-)}"
if [[ -z "$IMAGE" ]]; then
  echo "error: IMAGE not set in $SETTINGS" >&2
  exit 1
fi

echo "==> Building image: $IMAGE"
docker build --no-cache -t "$IMAGE" .

echo "==> Pushing image: $IMAGE"
docker push "$IMAGE"

echo "==> Applying overlay: $OVERLAY"
kubectl apply -k "$OVERLAY"

echo "==> Waiting for rollout"
kubectl rollout status deployment/k8sgpt-frontend -n k8sgpt-operator-system
