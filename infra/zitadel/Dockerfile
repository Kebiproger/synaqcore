# Минимальный образ для одноразового bootstrap Zitadel (POSIX sh + curl + jq).
FROM alpine:3.24

RUN apk add --no-cache curl jq \
    && adduser -D -H -u 1000 appuser

USER appuser
WORKDIR /tmp
