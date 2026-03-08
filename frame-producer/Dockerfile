FROM golang:1.22-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum* ./
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o /frame-producer .

FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends ffmpeg ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=builder /frame-producer /frame-producer
ENTRYPOINT ["/frame-producer"]
