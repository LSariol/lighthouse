# -- Build --
FROM golang:1.27.1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o lighthouse ./cmd/lighthouse

# -- Final --
# Pinned, so a rebuild gets the same base (and the same Compose CLI) as before.
# Bump it on purpose, e.g. for security fixes: alpine:3.24 follows 3.24.x.
FROM alpine:3.24
WORKDIR /app

# Lighthouse drives the host's Docker through the mounted socket, so it needs
# the Docker and Compose CLIs, and buildx (BuildKit) for test stages. It runs
# as root: the socket is root-equivalent anyway, and its group ID differs from
# host to host.
RUN apk add --no-cache docker-cli docker-cli-compose docker-cli-buildx

COPY --from=builder /app/lighthouse /lighthouse

# The daemon only; open the CLI with `docker exec -it lighthouse /lighthouse shell`.
CMD ["/lighthouse", "serve"]
