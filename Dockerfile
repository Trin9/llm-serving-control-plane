# Stage 1: The Builder Stage
FROM golang:1.24-alpine AS builder

# Set the working directory
WORKDIR /app

# Configure the Go proxy (to work around module download issues on some networks)
ENV GOPROXY=https://goproxy.cn,direct

# Copy go.mod and go.sum, then download dependencies; this leverages the Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the whole project source
COPY . .

# Build the Go application
# CGO_ENABLED=0: disables CGO so the produced binary is statically linked
# -a: force a rebuild of all dependencies (ensures a clean build)
# -ldflags "-s -w": strip symbol table and debug info to further reduce binary size
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-s -w" -o gate-service ./app/cmd

# Stage 2: The Final (Runtime) Stage
# Use the same distroless image as the Operator for maximum security (no shell) and minimal size
FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/gcr.io/distroless/static:nonroot

# Set the working directory to the filesystem root
WORKDIR /

# Copy the compiled binary from the builder stage
COPY --from=builder /app/gate-service .

# Run as the nonroot user (UID 65532)
USER 65532:65532

# Expose the service port (adjust to the port your Go application actually listens on)
EXPOSE 8080

# Define the command executed when the container starts
ENTRYPOINT ["/gate-service"]
