FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/k8sgpt-frontend ./cmd/k8sgpt-frontend

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/k8sgpt-frontend /k8sgpt-frontend
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/k8sgpt-frontend"]
