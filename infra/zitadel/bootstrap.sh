#!/bin/sh
# ==============================================================================
# Идемпотентный bootstrap Zitadel для SynaqCore (запускается сервисом zitadel-init).
# Безопасно перезапускать: существующие сущности пропускаются.
#
# Создаёт через Management API v1 (авторизация PAT сервисного аккаунта first instance):
#   - проект `synaqcore` (projectRoleAssertion=true) с ролями admin / analyst;
#   - OIDC-приложение `synaqcore-dashboard` (SPA, Auth Code + PKCE, JWT access token);
#   - грант роли admin первому human-администратору;
#   - machine user `synaq-automation` (JWT, client_credentials) с ролью admin.
#
# Выходные файлы (контракт для других сервисов):
#   $PUBLIC_DIR/oidc.json        {"issuer","project_id","client_id"}
#   $SECRETS_DIR/automation.json {"client_id","client_secret"}  (chmod 600)
#
# Bootstrap-PAT (IAM owner, пишет zitadel-setup при самом первом setup) удаляется
# (shred) после полностью успешного прогона. Повторный запуск без PAT, когда
# oidc.json и automation.json уже есть, завершается с кодом 0 без изменений.
# Чтобы применить изменённую конфигурацию (redirect URI, devMode ...) позже,
# создайте в консоли Zitadel новый PAT для `zitadel-bootstrap` (короткий срок),
# положите его в secrets/bootstrap.pat (chmod 600) и перезапустите zitadel-init.
#
# Zitadel определяет инстанс по Host, поэтому при обращении к http://zitadel:8080
# всегда отправляется заголовок Host: $ZITADEL_HOST (= внешний домен localhost:8081).
# ==============================================================================
set -eu
umask 077

ZITADEL_URL="${ZITADEL_URL:-http://zitadel:8080}"
ZITADEL_HOST="${ZITADEL_HOST:-localhost:8081}"
ZITADEL_ISSUER="${ZITADEL_ISSUER:-http://${ZITADEL_HOST}}"
PAT_FILE="${PAT_FILE:-/zitadel-secrets/bootstrap.pat}"
SECRETS_DIR="${SECRETS_DIR:-/zitadel-secrets}"
PUBLIC_DIR="${PUBLIC_DIR:-/zitadel-public}"
ADMIN_USERNAME="${ZITADEL_FIRSTINSTANCE_ORG_HUMAN_USERNAME:-zitadel-admin}"
DASHBOARD_REDIRECT_URI="${DASHBOARD_REDIRECT_URI:-http://localhost:8080/dashboard/callback}"
DASHBOARD_POST_LOGOUT_URI="${DASHBOARD_POST_LOGOUT_URI:-http://localhost:8080/dashboard/}"
# devMode разрешает http-redirect URI (нужно для локального http://localhost).
# В проде с https задайте ZITADEL_APP_DEVMODE=false.
APP_DEVMODE="${ZITADEL_APP_DEVMODE:-true}"
case "$APP_DEVMODE" in
    true | false) ;;
    *) echo "[zitadel-init] ОШИБКА: ZITADEL_APP_DEVMODE должен быть true или false" >&2; exit 1 ;;
esac

PROJECT_NAME="synaqcore"
APP_NAME="synaqcore-dashboard"
AUTOMATION_USER="synaq-automation"

# Каталог секретов доступен только владельцу
chmod 700 "$SECRETS_DIR" 2>/dev/null || true

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
RESP="$WORK/resp.json"
HDR="$WORK/auth.hdr"

log() { echo "[zitadel-init] $*"; }
die() { echo "[zitadel-init] ОШИБКА: $*" >&2; exit 1; }

OIDC_FILE="$PUBLIC_DIR/oidc.json"
AUTOMATION_FILE="$SECRETS_DIR/automation.json"

bootstrap_done() { # результаты прошлого успешного прогона на месте
    jq -e '(.issuer | length > 0) and (.project_id | length > 0) and (.client_id | length > 0)' \
        "$OIDC_FILE" > /dev/null 2>&1 \
        && jq -e '(.client_id | length > 0) and (.client_secret | length > 0)' \
            "$AUTOMATION_FILE" > /dev/null 2>&1
}

# --- PAT ------------------------------------------------------------------------
# zitadel-setup пишет PAT при первом setup (до старта zitadel, от которого мы зависим),
# поэтому ждём недолго — только на случай медленной ФС.
if [ ! -s "$PAT_FILE" ] && bootstrap_done; then
    log "bootstrap уже выполнен, PAT удалён — пропуск (oidc.json и automation.json на месте)"
    exit 0
fi
i=0
while [ ! -s "$PAT_FILE" ]; do
    i=$((i + 1))
    if [ "$i" -gt 15 ]; then
        die "PAT не найден: $PAT_FILE, а результаты прошлого bootstrap неполные
  ($OIDC_FILE и/или $AUTOMATION_FILE отсутствуют или повреждены).
  Как восстановить: в консоли Zitadel (http://${ZITADEL_HOST}/ui/console) создайте PAT
  для сервисного аккаунта zitadel-bootstrap с коротким сроком действия, сохраните его
  в infra/zitadel/secrets/bootstrap.pat (chmod 600) и перезапустите zitadel-init.
  Для полностью нового инстанса PAT создаётся автоматически при первом zitadel-setup."
    fi
    sleep 2
done
# Токен передаётся curl через файл заголовка, а не через аргументы командной строки
printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' < "$PAT_FILE")" > "$HDR"

# api METHOD PATH [JSON] -> тело ответа в $RESP, печатает HTTP-код
api() {
    if [ $# -ge 3 ]; then
        printf '%s' "$3" > "$WORK/body.json"
    else
        : > "$WORK/body.json"
    fi
    curl -sS -o "$RESP" -w '%{http_code}' -X "$1" "${ZITADEL_URL}$2" \
        -H "Host: ${ZITADEL_HOST}" \
        -H "@${HDR}" \
        -H 'Content-Type: application/json' \
        -H 'Accept: application/json' \
        --data-binary "@$WORK/body.json"
}

# call METHOD PATH [JSON] -> тело ответа в stdout; падает на не-2xx.
# Используется только как X="$(call ...)" или call ... >file, чтобы set -e ловил ошибки
# (в конвейере `call | jq` код возврата call теряется).
call() {
    code="$(api "$@")" || die "$1 $2: запрос не выполнен"
    case "$code" in
        2??) cat "$RESP" ;;
        *) die "$1 $2 -> HTTP $code: $(jq -c '{code,message}' "$RESP" 2>/dev/null || head -c 300 "$RESP")" ;;
    esac
}

eq_query() { # eq_query <queryName> <fieldName> <value>
    jq -nc --arg q "$1" --arg f "$2" --arg v "$3" \
        '{queries: [{($q): {($f): $v, method: "TEXT_QUERY_METHOD_EQUALS"}}]}'
}

# --- Ожидание готовности API ---------------------------------------------------
i=0
while :; do
    code="$(api GET /management/v1/orgs/me || true)"
    [ "$code" = "200" ] && break
    case "$code" in
        401 | 403) die "PAT отклонён (HTTP $code): истёк или отозван. Создайте новый PAT для
  zitadel-bootstrap в консоли Zitadel, сохраните в infra/zitadel/secrets/bootstrap.pat
  (chmod 600) и перезапустите zitadel-init." ;;
    esac
    i=$((i + 1))
    [ "$i" -gt 60 ] && die "Management API недоступен (последний HTTP-код: ${code:-нет})"
    sleep 2
done
log "Management API доступен"

# --- Проект ---------------------------------------------------------------------
PROJECT_JSON="$(call POST /management/v1/projects/_search "$(eq_query nameQuery name "$PROJECT_NAME")")"
PROJECT_ID="$(printf '%s' "$PROJECT_JSON" | jq -r '.result[0].id // empty')"
if [ -z "$PROJECT_ID" ]; then
    R="$(call POST /management/v1/projects \
        "$(jq -nc --arg n "$PROJECT_NAME" '{name: $n, projectRoleAssertion: true}')")"
    PROJECT_ID="$(printf '%s' "$R" | jq -r '.id // empty')"
    log "Проект $PROJECT_NAME создан"
else
    if [ "$(printf '%s' "$PROJECT_JSON" | jq -r '.result[0].projectRoleAssertion // false')" != "true" ]; then
        call PUT "/management/v1/projects/$PROJECT_ID" \
            "$(jq -nc --arg n "$PROJECT_NAME" '{name: $n, projectRoleAssertion: true}')" > /dev/null
        log "Проект $PROJECT_NAME: включён projectRoleAssertion"
    else
        log "Проект $PROJECT_NAME уже существует — пропуск"
    fi
fi
[ -n "$PROJECT_ID" ] || die "не удалось получить project_id"

# --- Роли проекта ---------------------------------------------------------------
ensure_role() { # ensure_role <key> <displayName>
    R="$(call POST "/management/v1/projects/$PROJECT_ID/roles/_search" "$(eq_query keyQuery key "$1")")"
    n="$(printf '%s' "$R" | jq '.result // [] | length')"
    if [ "$n" -eq 0 ]; then
        call POST "/management/v1/projects/$PROJECT_ID/roles" \
            "$(jq -nc --arg k "$1" --arg d "$2" '{roleKey: $k, displayName: $d}')" > /dev/null
        log "Роль $1 создана"
    else
        log "Роль $1 уже существует — пропуск"
    fi
}
ensure_role admin "Administrator"
ensure_role analyst "Analyst"

# --- OIDC-приложение (SPA, PKCE) -------------------------------------------------
# Желаемая OIDC-конфигурация. Если приложение уже есть и конфиг отличается —
# обновляем его (PUT oidc_config), иначе пропускаем (Zitadel отвечает ошибкой на
# «пустое» обновление без изменений).
APP_CFG="$(jq -nc --arg r "$DASHBOARD_REDIRECT_URI" --arg p "$DASHBOARD_POST_LOGOUT_URI" \
    --argjson dev "$APP_DEVMODE" '{
    redirectUris: [$r],
    postLogoutRedirectUris: [$p],
    responseTypes: ["OIDC_RESPONSE_TYPE_CODE"],
    grantTypes: ["OIDC_GRANT_TYPE_AUTHORIZATION_CODE", "OIDC_GRANT_TYPE_REFRESH_TOKEN"],
    appType: "OIDC_APP_TYPE_USER_AGENT",
    authMethodType: "OIDC_AUTH_METHOD_TYPE_NONE",
    devMode: $dev,
    accessTokenType: "OIDC_TOKEN_TYPE_JWT",
    accessTokenRoleAssertion: true,
    idTokenRoleAssertion: true
}')"
R="$(call POST "/management/v1/projects/$PROJECT_ID/apps/_search" "$(eq_query nameQuery name "$APP_NAME")")"
CLIENT_ID="$(printf '%s' "$R" | jq -r '.result[0].oidcConfig.clientId // empty')"
if [ -z "$CLIENT_ID" ]; then
    R="$(call POST "/management/v1/projects/$PROJECT_ID/apps/oidc" "$(printf '%s' "$APP_CFG" | jq -c \
        --arg n "$APP_NAME" '. + {name: $n, version: "OIDC_VERSION_1_0"}')")"
    CLIENT_ID="$(printf '%s' "$R" | jq -r '.clientId // empty')"
    log "OIDC-приложение $APP_NAME создано"
else
    APP_ID="$(printf '%s' "$R" | jq -r '.result[0].id')"
    # Сравниваем только управляемые поля (массивы — без учёта порядка)
    if printf '%s' "$R" | jq -e --argjson want "$APP_CFG" '
        def n: if type == "array" then sort else . end;
        .result[0].oidcConfig as $cur
        | [$want | to_entries[]
           | (.value | n) == (($cur[.key] // (if (.value | type) == "boolean" then false else null end)) | n)]
        | all
    ' > /dev/null; then
        log "OIDC-приложение $APP_NAME уже существует, конфиг актуален — пропуск"
    else
        call PUT "/management/v1/projects/$PROJECT_ID/apps/$APP_ID/oidc_config" "$APP_CFG" > /dev/null
        log "OIDC-приложение $APP_NAME: конфиг обновлён"
    fi
fi
[ -n "$CLIENT_ID" ] || die "не удалось получить client_id приложения"

# --- Гранты ролей ---------------------------------------------------------------
find_user() { # find_user <userName> -> userId в $FOUND_ID (пусто, если нет)
    R="$(call POST /management/v1/users/_search "$(eq_query userNameQuery userName "$1")")"
    FOUND_ID="$(printf '%s' "$R" | jq -r '.result[0].id // empty')"
}

ensure_grant() { # ensure_grant <userId> <roleKey>
    G="$(call POST /management/v1/users/grants/_search "$(jq -nc --arg u "$1" --arg p "$PROJECT_ID" \
        '{queries: [{userIdQuery: {userId: $u}}, {projectIdQuery: {projectId: $p}}]}')")"
    GRANT_ID="$(printf '%s' "$G" | jq -r '.result[0].id // empty')"
    if [ -z "$GRANT_ID" ]; then
        call POST "/management/v1/users/$1/grants" \
            "$(jq -nc --arg p "$PROJECT_ID" --arg r "$2" '{projectId: $p, roleKeys: [$r]}')" > /dev/null
        log "Грант $2 выдан пользователю $1"
    elif printf '%s' "$G" | jq -e --arg r "$2" '.result[0].roleKeys // [] | index($r)' > /dev/null; then
        log "Грант $2 у пользователя $1 уже есть — пропуск"
    else
        call PUT "/management/v1/users/$1/grants/$GRANT_ID" "$(printf '%s' "$G" | jq -c --arg r "$2" \
            '{roleKeys: ((.result[0].roleKeys // []) + [$r] | unique)}')" > /dev/null
        log "Грант пользователя $1 дополнен ролью $2"
    fi
}

# Zitadel дописывает к username первого human-администратора домен организации
# (zitadel-admin -> zitadel-admin@synaqcore.localhost), поэтому ищем оба варианта.
find_user "$ADMIN_USERNAME"
ADMIN_ID="$FOUND_ID"
if [ -z "$ADMIN_ID" ]; then
    R="$(call POST /management/v1/users/_search "$(jq -nc --arg v "${ADMIN_USERNAME}@" \
        '{queries: [{userNameQuery: {userName: $v, method: "TEXT_QUERY_METHOD_STARTS_WITH"}}, {typeQuery: {type: "TYPE_HUMAN"}}]}')")"
    ADMIN_ID="$(printf '%s' "$R" | jq -r '.result[0].id // empty')"
fi
[ -n "$ADMIN_ID" ] || die "human-администратор '$ADMIN_USERNAME' не найден"
ensure_grant "$ADMIN_ID" admin

# --- Machine user для скриптов / locust (client_credentials) ---------------------
find_user "$AUTOMATION_USER"
AUTO_ID="$FOUND_ID"
NEED_SECRET=0
if [ -z "$AUTO_ID" ]; then
    R="$(call POST /management/v1/users/machine "$(jq -nc --arg u "$AUTOMATION_USER" '{
        userName: $u,
        name: "SynaqCore automation",
        description: "client_credentials для скриптов и нагрузочных тестов",
        accessTokenType: "ACCESS_TOKEN_TYPE_JWT"
    }')")"
    AUTO_ID="$(printf '%s' "$R" | jq -r '.userId // empty')"
    log "Machine user $AUTOMATION_USER создан"
    NEED_SECRET=1
else
    log "Machine user $AUTOMATION_USER уже существует — пропуск"
    if ! jq -e '(.client_id | length > 0) and (.client_secret | length > 0)' "$AUTOMATION_FILE" > /dev/null 2>&1; then
        NEED_SECRET=1
    fi
fi
[ -n "$AUTO_ID" ] || die "не удалось получить id machine user"

if [ "$NEED_SECRET" -eq 1 ]; then
    # Ответ содержит секрет — пишем сразу в файл (umask 077 -> 600), в лог не выводим
    call PUT "/management/v1/users/$AUTO_ID/secret" '{}' > "$WORK/secret.json"
    jq '{client_id: .clientId, client_secret: .clientSecret}' "$WORK/secret.json" > "$WORK/automation.json"
    rm -f "$WORK/secret.json"
    jq -e '(.client_id | length > 0) and (.client_secret | length > 0)' "$WORK/automation.json" > /dev/null \
        || die "ответ на генерацию секрета не содержит clientId/clientSecret"
    mv -f "$WORK/automation.json" "$AUTOMATION_FILE"
    log "Client secret для $AUTOMATION_USER сгенерирован -> automation.json"
fi
chmod 600 "$AUTOMATION_FILE" "$PAT_FILE" 2>/dev/null || true
ensure_grant "$AUTO_ID" admin

# --- Публичная конфигурация (без секретов) ----------------------------------------
jq -n --arg i "$ZITADEL_ISSUER" --arg p "$PROJECT_ID" --arg c "$CLIENT_ID" \
    '{issuer: $i, project_id: $p, client_id: $c}' > "$WORK/oidc.json"
chmod 644 "$WORK/oidc.json"
mv -f "$WORK/oidc.json" "$OIDC_FILE"
log "Записан $OIDC_FILE"

# --- Удаление bootstrap-PAT -------------------------------------------------------
# Сюда доходим только если все шаги выше успешны (set -e + die). PAT — учётка
# IAM owner, на диске после bootstrap она не нужна (get_token.sh использует automation.json).
bootstrap_done || die "проверка oidc.json / automation.json не прошла — PAT не удалён"
if command -v shred > /dev/null 2>&1; then
    shred -u "$PAT_FILE" 2>/dev/null || rm -f "$PAT_FILE"
else
    rm -f "$PAT_FILE"
fi
[ ! -e "$PAT_FILE" ] || die "не удалось удалить $PAT_FILE"
log "Bootstrap-PAT удалён ($PAT_FILE)"
log "Готово"
