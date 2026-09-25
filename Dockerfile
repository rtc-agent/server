# Build stage
FROM golang:1.27-alpine AS builder

WORKDIR /app

# Install dependencies
RUN apk add --no-cache git

# Copy root go mod files + local module go.mod files (for replace directives)
COPY go.mod go.sum ./
COPY pkg/centrifuge-plus/go.mod pkg/centrifuge-plus/go.sum ./pkg/centrifuge-plus/
COPY pkg/protocol/go.mod ./pkg/protocol/
COPY pkg/rtc-queue/go.mod pkg/rtc-queue/go.sum ./pkg/rtc-queue/
RUN go mod download

# Copy source code
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o rtc-agent .

# Final stage
FROM alpine:latest

WORKDIR /app

# Install ca-certificates for HTTPS
RUN apk --no-cache add ca-certificates tzdata

# Copy binary and config
COPY --from=builder /app/rtc-agent .
COPY --from=builder /app/etc ./etc

# Expose port
EXPOSE 8888

# Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:8888/healthz || exit 1

# Run
CMD ["./rtc-agent", "serve"]
