# syntax=docker/dockerfile:1
# ---- build stage ----
FROM golang:1.23-alpine AS builder
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum* ./
RUN go mod download

# Copy source and build a static-ish binary.
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/opencode-sw .

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata sqlite-libs && \
    addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /out/opencode-sw /app/opencode-sw

# Persistent data lives here (mount a Zeabur persistent volume on /data).
RUN mkdir -p /data && chown -R app:app /data /app
USER app
ENV DB_PATH=/data/opencode-sw.db \
    PORT=3000
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:${PORT}/health || exit 1

ENTRYPOINT ["/app/opencode-sw"]
