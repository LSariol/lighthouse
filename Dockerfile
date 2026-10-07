FROM golang:1.27.1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o lighthouse ./cmd/lighthouse

# Pinned on purpose; bump it deliberately (e.g. for security fixes).
FROM alpine:3.24
WORKDIR /app

# Runs as root: the Docker socket is root-equivalent anyway.
RUN apk add --no-cache docker-cli docker-cli-compose docker-cli-buildx

COPY --from=builder /app/lighthouse /lighthouse

CMD ["/lighthouse", "serve"]
