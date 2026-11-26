# syntax=docker/dockerfile:1
# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /civ6relay-server \
    ./cmd/server

# Runtime stage
FROM alpine:3
COPY --from=builder /civ6relay-server /civ6relay-server
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh /civ6relay-server
EXPOSE 42056/udp
ENTRYPOINT ["/docker-entrypoint.sh"]
