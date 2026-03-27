FROM golang:1.25-bookworm AS builder

WORKDIR /app

ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/bridge ./cmd/bridge

FROM golang:1.25-bookworm AS runtime

WORKDIR /app

COPY --from=builder /out/bridge /usr/local/bin/bridge
COPY .env.example /app/.env.example

RUN mkdir -p /app/logs \
    && useradd -m -u 10001 appuser \
    && chown -R appuser:appuser /app

USER appuser

EXPOSE 8083

ENTRYPOINT ["bridge"]
