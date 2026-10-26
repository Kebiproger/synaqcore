FROM golang:1.27.3-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /build/synaq ./cmd/synaq

# Минимальный образ
FROM alpine:3.24

RUN apk --no-cache add ca-certificates wget && \
    addgroup -S appgroup && adduser -S appuser -G appgroup

COPY --from=builder /build/synaq /synaq

USER appuser

EXPOSE 8000

CMD ["/synaq"]
