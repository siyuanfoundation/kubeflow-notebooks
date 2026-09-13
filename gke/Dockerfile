FROM golang:1.26.6 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /access-proxy ./cmd/access-proxy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /access-proxy /access-proxy
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/access-proxy"]
