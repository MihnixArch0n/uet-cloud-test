# Multi-stage Dockerfile for uet-cloud-test
# Stage 1: Build
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY main.go ./
COPY static/ ./static/

# Compile statically linked binary with optimizations
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o uet-cloud-test main.go

# Stage 2: Runtime
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/uet-cloud-test /app/uet-cloud-test

# Run as non-root user for security
RUN adduser -D -u 10001 appuser
USER 10001:10001

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/uet-cloud-test"]
