# Build stage - Alpine based
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache ca-certificates

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN go build -o voiceactivitybot .

# Final stage - Distroless non-root
FROM gcr.io/distroless/static:nonroot

# Copy CA certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary from builder
COPY --from=builder /build/voiceactivitybot /voiceactivitybot

# Use non-root user (default in distroless/static:nonroot is uid 65532)
USER nonroot:nonroot

# Set working directory
WORKDIR /

# Run the application
ENTRYPOINT ["/voiceactivitybot"]
