FROM golang:alpine AS builder

WORKDIR /app

COPY . .

# Resolve Go module checksums during the image build.
RUN go mod download

# Build the application
RUN go build -ldflags="-s -w" -o local-content-share .

# Use a minimal alpine image for running
FROM alpine:latest

WORKDIR /app

# Create data directory if not exists
RUN mkdir -p /app/data

# Copy the binary from builder
COPY --from=builder /app/local-content-share .

# Expose the default port
EXPOSE 8080

# Run the server
CMD ["/app/local-content-share"]
