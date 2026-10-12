FROM golang:1.27.1-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /build/track_api ./cmd/api

# Минимальный образ
FROM alpine:3.24

WORKDIR /app
COPY --from=builder /build/track_api /app/track_api

EXPOSE 8000

CMD ["/app/track_api"]