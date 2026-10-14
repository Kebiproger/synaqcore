#!/bin/sh
# ==============================================================================
# Получение access token (JWT) сервисного аккаунта synaq-automation через
# OAuth2 client_credentials. Печатает только токен в stdout.
#
#   TOKEN="$(scripts/get_token.sh)"
#   curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/...
#
# Требует: curl, jq. Файлы создаёт сервис zitadel-init:
#   infra/zitadel/secrets/automation.json, infra/zitadel/public/oidc.json
# ==============================================================================
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SECRETS_FILE="${AUTOMATION_FILE:-$ROOT/infra/zitadel/secrets/automation.json}"
OIDC_FILE="${OIDC_CONFIG_FILE:-$ROOT/infra/zitadel/public/oidc.json}"

for f in "$SECRETS_FILE" "$OIDC_FILE"; do
    [ -r "$f" ] || { echo "Файл не найден: $f (запущен ли zitadel-init?)" >&2; exit 1; }
done

ISSUER="${OIDC_ISSUER:-$(jq -r '.issuer' "$OIDC_FILE")}"
PROJECT_ID="$(jq -r '.project_id' "$OIDC_FILE")"
SCOPE="openid urn:zitadel:iam:org:project:id:${PROJECT_ID}:aud urn:zitadel:iam:org:projects:roles"

# Учётные данные передаются curl через stdin-конфиг, чтобы не светиться в `ps`
RESP="$(jq -r '"user = \"" + .client_id + ":" + .client_secret + "\""' "$SECRETS_FILE" \
    | curl -sS --fail-with-body -K - -X POST "${ISSUER}/oauth/v2/token" \
        -H 'Content-Type: application/x-www-form-urlencoded' \
        --data-urlencode 'grant_type=client_credentials' \
        --data-urlencode "scope=${SCOPE}")" || {
    echo "Не удалось получить токен: $(printf '%s' "${RESP:-}" | jq -c '{error,error_description}' 2>/dev/null)" >&2
    exit 1
}

TOKEN="$(printf '%s' "$RESP" | jq -r '.access_token // empty')"
[ -n "$TOKEN" ] || { echo "В ответе нет access_token" >&2; exit 1; }
printf '%s\n' "$TOKEN"
