# Multi-arch: docker buildx build --platform linux/amd64,linux/arm64 .
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/k8sgpt-frontend ./cmd/k8sgpt-frontend

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/k8sgpt-frontend /k8sgpt-frontend
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/doc/k8sgpt-frontend/
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/k8sgpt-frontend"]
