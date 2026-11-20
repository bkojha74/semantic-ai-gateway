# Stage 1: Build binary
FROM golang:1.24-alpine AS builder

WORKDIR /src

# Download dependencies first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /out/gateway ./cmd/gateway

# Stage 2: Minimal distroless runtime
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/gateway /app/gateway

ENV PORT=8080
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/app/gateway"]