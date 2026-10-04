#!/usr/bin/env bash
# Run the tests, then build and push a multi-arch image.
#   ./build.sh [tag]
# Env: IMAGE (default ghcr.io/opswhisperer/k8sgpt-frontend), PLATFORMS (default linux/amd64,linux/arm64).
set -euo pipefail
cd "$(dirname "$0")"

IMAGE=${IMAGE:-ghcr.io/opswhisperer/k8sgpt-frontend}
PLATFORMS=${PLATFORMS:-linux/amd64,linux/arm64}
TAG=${1:-$(git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)}

go vet ./...
go test ./...

docker buildx build \
  --platform "$PLATFORMS" \
  --build-arg "VERSION=${TAG}" \
  --push \
  -t "${IMAGE}:${TAG}" \
  -t "${IMAGE}:latest" \
  .

echo "Pushed ${IMAGE}:${TAG} (and :latest) for ${PLATFORMS}"
