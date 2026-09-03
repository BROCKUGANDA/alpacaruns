# Alpacaruns — multi-stage build. Final image: non-root, binary only.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/alpacaruns ./cmd/alpacaruns

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/alpacaruns /app/alpacaruns
# Paper trading by default; keys are injected at runtime (see DEPLOY.md).
ENV ALPACA_BASE_URL=https://paper-api.alpaca.markets/v2 \
    TRADE_LOG=/app/data/trades.jsonl
VOLUME ["/app/data"]
ENTRYPOINT ["/app/alpacaruns"]
