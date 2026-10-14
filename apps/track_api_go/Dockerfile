FROM golang:1.27.1-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /build/track_api ./cmd/api

# Минимальный образ
FROM alpine:3.24

RUN apk --no-cache add ca-certificates wget && \
    addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app
COPY --from=builder /build/track_api /app/track_api
RUN chown -R appuser:appgroup /app

USER appuser

EXPOSE 8000

CMD ["/app/track_api"]