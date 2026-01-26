FROM golang:1.24.5-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files to the working directory
COPY go.mod go.sum ./

# Download the dependencies
RUN go mod download

# Copy the rest of the application code to the working directory
COPY . .

# Build the Go application
RUN CGO_ENABLED=0 GOOS=linux go build -o /snapshotter-local-collector ./cmd/main.go

# Use alpine base image for healthcheck tools (curl)
FROM alpine:latest

# Install curl for healthcheck
RUN apk add --no-cache curl ca-certificates

# Copy SSL certificates from the builder stage
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary from the builder stage
COPY --from=builder /snapshotter-local-collector /snapshotter-local-collector

# Command to run the application
CMD ["/snapshotter-local-collector"]
