FROM golang:1.24-alpine AS builder
RUN apk add --no-cache curl

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Bake the design-system bundle into the embed tree before `go build` so
# //go:embed picks it up. DS_REF lets you pin to a tag; defaults to main.
# To force a cache-miss on this layer pass --build-arg DS_CACHEBUST=$(date +%s).
ARG DS_REF=main
ARG DS_CACHEBUST=
RUN mkdir -p internal/viz/static/assets/vendor && \
    echo "cachebust: ${DS_CACHEBUST}" > /dev/null && \
    curl -fsSL "https://cdn.jsdelivr.net/gh/dzarlax/design-system@${DS_REF}/dist/dzarlax.css" \
        -o internal/viz/static/assets/vendor/dzarlax.css && \
    curl -fsSL "https://cdn.jsdelivr.net/gh/dzarlax/design-system@${DS_REF}/dist/dzarlax.js" \
        -o internal/viz/static/assets/vendor/dzarlax.js

RUN CGO_ENABLED=0 go build -o /personal-memory ./cmd/server
RUN CGO_ENABLED=0 go build -o /personal-memory-indexer ./cmd/indexer

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /personal-memory /personal-memory
COPY --from=builder /personal-memory-indexer /personal-memory-indexer

ENTRYPOINT ["/personal-memory"]
