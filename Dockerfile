FROM golang:1.25-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/bridge ./cmd/bridge

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/bridge /usr/local/bin/bridge
COPY .env.example /app/.env.example

RUN mkdir -p /app/logs \
    && useradd -m -u 10001 appuser \
    && chown -R appuser:appuser /app

USER appuser

EXPOSE 8083

ENTRYPOINT ["bridge"]
