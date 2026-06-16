FROM golang:1.24 AS builder

WORKDIR /app

COPY . .

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o assetd assetd.go

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y ffmpeg ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/assetd .
COPY fallback-games.txt .

CMD ["./assetd"]
