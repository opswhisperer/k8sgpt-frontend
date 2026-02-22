APP := k8sgpt-frontend
PKG := ./cmd/k8sgpt-frontend
BINARY := bin/$(APP)

GO ?= $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod

IMAGE ?= ghcr.io/opswhisperer/k8sgpt-frontend:latest
KUSTOMIZE_OVERLAY ?= deploy/overlays/local

.PHONY: help tidy build run clean docker-build docker-push deploy

help:
	@echo "Targets:"
	@echo "  make tidy         - go mod tidy"
	@echo "  make build        - build local binary to $(BINARY)"
	@echo "  make run          - run frontend locally"
	@echo "  make clean        - remove build/cache artifacts"
	@echo "  make docker-build - build container image (set IMAGE=...)"
	@echo "  make docker-push  - push container image (set IMAGE=...)"
	@echo "  make deploy       - kubectl apply -k overlay (set KUSTOMIZE_OVERLAY=...)"

tidy:
	@mkdir -p $(GOCACHE) $(GOMODCACHE) bin
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) mod tidy

build:
	@mkdir -p $(GOCACHE) $(GOMODCACHE) bin
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build -o $(BINARY) $(PKG)

run:
	@mkdir -p $(GOCACHE) $(GOMODCACHE)
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) run $(PKG)

clean:
	rm -rf bin .cache

docker-build:
	docker build -t $(IMAGE) .

docker-push:
	docker push $(IMAGE)

deploy:
	@if [ ! -f "$(KUSTOMIZE_OVERLAY)/settings.env" ]; then echo "missing $(KUSTOMIZE_OVERLAY)/settings.env (copy from settings.env.example)"; exit 1; fi
	kubectl apply -k $(KUSTOMIZE_OVERLAY)
