FROM golang:1.22-bookworm AS builder

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download || true
RUN go mod tidy
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /frame-producer .

# ── Runtime: slim image with just ffmpeg ──
FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ffmpeg ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /frame-producer /frame-producer

ENTRYPOINT ["/frame-producer"]
