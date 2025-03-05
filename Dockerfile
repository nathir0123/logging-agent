# Use Go 1.23.0 as the base image
FROM golang:1.23.0 AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy the Go module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the entire project
COPY . .

# Build the Go application
RUN go build -o logging-agent main.go

# Use a minimal base image for running the app (optional)
FROM debian:latest
WORKDIR /app
COPY --from=builder /app/logging-agent .

# Set the command to run the application
CMD ["/app/logging-agent"]
