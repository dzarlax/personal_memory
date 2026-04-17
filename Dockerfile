FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /personal-memory ./cmd/server
RUN CGO_ENABLED=0 go build -o /personal-memory-indexer ./cmd/indexer

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /personal-memory /personal-memory
COPY --from=builder /personal-memory-indexer /personal-memory-indexer

ENTRYPOINT ["/personal-memory"]
