#!/bin/sh
# ==============================================================================
# Роли бизнес-БД postgres-app (одноразовый сервис postgres-app-init).
#
# Запускается при КАЖДОМ `docker compose up` и идемпотентен: на пустом томе,
# на томе, созданном прежней версией (таблицы и функции принадлежат
# суперпользователю), и при повторных запусках.
#
#   synaq_app        — владелец схем public и events (эксперименты, goose,
#                      события); без SUPERUSER/CREATEROLE/CREATEDB/REPLICATION/
#                      BYPASSRLS; CONNECTION LIMIT 15.
#   synaq_analytics  — только чтение схемы events (default_transaction_read_only,
#                      statement_timeout=15s); CONNECTION LIMIT 6; к public доступа нет.
#
# Учётка суперпользователя (POSTGRES_*) есть только в этом контейнере.
# Пароли ролей читаются psql из окружения (\getenv), не попадают ни в argv,
# ни в вывод, ни в журнал сервера.
# ==============================================================================
set -eu

APP_ROLE=synaq_app
ANALYTICS_ROLE=synaq_analytics
MIN_PW_LEN=16

fail() {
    echo "postgres-app-init: $*" >&2
    exit 1
}

: "${PGHOST:?задайте PGHOST}"
: "${PGUSER:?задайте PGUSER (POSTGRES_USER)}"
: "${PGPASSWORD:?задайте PGPASSWORD (POSTGRES_PASSWORD)}"
: "${PGDATABASE:?задайте PGDATABASE (POSTGRES_DB)}"

# Пароли: непустые и не короче MIN_PW_LEN символов
SYNAQ_DB_PASSWORD="${SYNAQ_DB_PASSWORD:-}"
ANALYTICS_DB_PASSWORD="${ANALYTICS_DB_PASSWORD:-}"
[ "${#SYNAQ_DB_PASSWORD}" -ge "$MIN_PW_LEN" ] \
    || fail "SYNAQ_DB_PASSWORD пуст или короче $MIN_PW_LEN символов (infra/.env.infra)"
[ "${#ANALYTICS_DB_PASSWORD}" -ge "$MIN_PW_LEN" ] \
    || fail "ANALYTICS_DB_PASSWORD пуст или короче $MIN_PW_LEN символов (infra/.env.infra)"
[ "$SYNAQ_DB_PASSWORD" != "$ANALYTICS_DB_PASSWORD" ] \
    || fail "SYNAQ_DB_PASSWORD и ANALYTICS_DB_PASSWORD должны различаться"

# Суперпользователь не должен совпадать с прикладными ролями: иначе ALTER ROLE
# ниже снял бы с него SUPERUSER
case "$PGUSER" in
    "$APP_ROLE" | "$ANALYTICS_ROLE")
        fail "POSTGRES_USER=$PGUSER совпадает с прикладной ролью; задайте другое имя суперпользователя"
        ;;
esac

export SYNAQ_DB_PASSWORD ANALYTICS_DB_PASSWORD

# Одна транзакция: либо всё применено, либо ничего
psql -X -q -v ON_ERROR_STOP=1 --single-transaction \
    -v app_role="$APP_ROLE" -v analytics_role="$ANALYTICS_ROLE" -v dbname="$PGDATABASE" <<'SQL'
-- Текст ALTER ROLE ... PASSWORD не должен попасть в журнал сервера даже при ошибке
SET LOCAL log_statement = 'none';
SET LOCAL log_min_error_statement = 'panic';
-- Не висим бесконечно, если synaq держит блокировку на таблице
SET LOCAL lock_timeout = '10s';
SET LOCAL client_min_messages = warning;

\getenv synaq_pw SYNAQ_DB_PASSWORD
\getenv analytics_pw ANALYTICS_DB_PASSWORD

-- --- Роли -------------------------------------------------------------------
SELECT NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app_role') AS need_app,
       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'analytics_role') AS need_analytics
\gset
\if :need_app
CREATE ROLE :"app_role";
\endif
\if :need_analytics
CREATE ROLE :"analytics_role";
\endif

ALTER ROLE :"app_role" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
    NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT 15 PASSWORD :'synaq_pw';
ALTER ROLE :"analytics_role" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
    NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT 6 PASSWORD :'analytics_pw';
ALTER ROLE :"analytics_role" SET default_transaction_read_only = on;
ALTER ROLE :"analytics_role" SET statement_timeout = '15s';

\unset synaq_pw
\unset analytics_pw

-- --- База -------------------------------------------------------------------
REVOKE ALL ON DATABASE :"dbname" FROM PUBLIC;
GRANT CONNECT, TEMPORARY, CREATE ON DATABASE :"dbname" TO :"app_role";
GRANT CONNECT ON DATABASE :"dbname" TO :"analytics_role";

-- --- Схемы ------------------------------------------------------------------
ALTER SCHEMA public OWNER TO :"app_role";
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS events AUTHORIZATION :"app_role";
ALTER SCHEMA events OWNER TO :"app_role";
REVOKE ALL ON SCHEMA events FROM PUBLIC;

-- --- Объекты, созданные под суперпользователем (до появления ролей) ----------
-- Владельцем становится synaq_app (goose и synaq меняют схему под ним).
-- ALTER выполняется только при несовпадении владельца: в штатном режиме
-- AccessExclusive-блокировки не берутся. Пропускаются объекты расширений
-- (deptype 'e'), sequence, привязанные к столбцам (serial — 'a', identity — 'i':
-- переходят вместе с таблицей), индексы и TOAST.
SELECT format('ALTER %s %I.%I OWNER TO %I',
              CASE c.relkind
                  WHEN 'S' THEN 'SEQUENCE'
                  WHEN 'v' THEN 'VIEW'
                  WHEN 'm' THEN 'MATERIALIZED VIEW'
                  WHEN 'f' THEN 'FOREIGN TABLE'
                  ELSE 'TABLE'
              END,
              n.nspname, c.relname, :'app_role')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN ('public', 'events')
  AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
  AND c.relowner <> (SELECT oid FROM pg_roles WHERE rolname = :'app_role')
  AND NOT EXISTS (
      SELECT 1 FROM pg_depend d
      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
        AND d.deptype IN ('e', 'a', 'i')
  )
ORDER BY n.nspname, c.relname
\gexec

-- Функции и процедуры (experiments_notify и т.п.)
SELECT format('ALTER ROUTINE %I.%I(%s) OWNER TO %I',
              n.nspname, p.proname, pg_get_function_identity_arguments(p.oid), :'app_role')
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname IN ('public', 'events')
  AND p.prokind IN ('f', 'p')
  AND p.proowner <> (SELECT oid FROM pg_roles WHERE rolname = :'app_role')
  AND NOT EXISTS (
      SELECT 1 FROM pg_depend d
      WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, p.proname
\gexec

-- Самостоятельные типы (enum, domain); составные типы таблиц и массивы
-- переходят вместе со своими объектами
SELECT format('ALTER %s %I.%I OWNER TO %I',
              CASE t.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END,
              n.nspname, t.typname, :'app_role')
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname IN ('public', 'events')
  AND t.typtype IN ('e', 'd')
  AND t.typowner <> (SELECT oid FROM pg_roles WHERE rolname = :'app_role')
  AND NOT EXISTS (
      SELECT 1 FROM pg_depend d
      WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, t.typname
\gexec

-- --- synaq_analytics: только чтение events ------------------------------------
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM :"analytics_role";
GRANT USAGE ON SCHEMA events TO :"analytics_role";
GRANT SELECT ON ALL TABLES IN SCHEMA events TO :"analytics_role";
-- Таблицы и партиции, которые synaq_app создаст позже
ALTER DEFAULT PRIVILEGES FOR ROLE :"app_role" IN SCHEMA events
    GRANT SELECT ON TABLES TO :"analytics_role";
SQL

echo "postgres-app-init: роли $APP_ROLE и $ANALYTICS_ROLE настроены"
