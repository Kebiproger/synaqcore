#!/usr/bin/env bash
# Smoke-тесты Go-сервиса synaq: поднять стенд, дождаться готовности, прогнать pytest, снести стенд.
#
#   tests/contract/run.sh                  # весь набор
#   tests/contract/run.sh -k variants -x   # аргументы передаются в pytest
#   SMOKE_KEEP=1 tests/contract/run.sh     # не сносить стенд после прогона
#   SMOKE_EVENT_SINK=clickhouse tests/contract/run.sh   # события в ClickHouse (make smoke-ch)
#
# Переменные: PYTHON (по умолчанию ./venv/bin/python или python3), SYNAQ_PORT (18002),
# SYNAQ_BASE_URL (по умолчанию http://127.0.0.1:$SYNAQ_PORT), SMOKE_KEEP,
# SMOKE_EVENT_SINK (postgres по умолчанию | clickhouse).
# Стенд — отдельный compose-проект synaq-contract, основной стек не затрагивается.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Хранилище событий: postgres (по умолчанию) или clickhouse (профиль clickhouse стенда)
SMOKE_EVENT_SINK="${SMOKE_EVENT_SINK:-postgres}"
case "$SMOKE_EVENT_SINK" in
    postgres) PROFILE_ARGS=() ;;
    clickhouse) PROFILE_ARGS=(--profile clickhouse) ;;
    *) echo "SMOKE_EVENT_SINK должен быть postgres или clickhouse, получено: $SMOKE_EVENT_SINK" >&2; exit 2 ;;
esac
export SMOKE_EVENT_SINK
# Профиль нужен и для `down`, иначе контейнер clickhouse остался бы после прогона
COMPOSE=(docker compose -f "$SCRIPT_DIR/docker-compose.contract.yml" -p synaq-contract ${PROFILE_ARGS[@]+"${PROFILE_ARGS[@]}"})

if [[ -z "${PYTHON:-}" ]]; then
    if [[ -x "$ROOT_DIR/venv/bin/python" ]]; then PYTHON="$ROOT_DIR/venv/bin/python"; else PYTHON=python3; fi
fi

export SYNAQ_SMOKE=1
export SYNAQ_BASE_URL="${SYNAQ_BASE_URL:-http://127.0.0.1:${SYNAQ_PORT:-18002}}"

cleanup() {
    local rc=$?
    if [[ "${SMOKE_KEEP:-0}" != "1" ]]; then
        echo ">>> Сносим smoke-стенд"
        "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
    fi
    exit $rc
}
trap cleanup EXIT INT TERM

# Каталог для jwks.json должен существовать до старта (иначе docker создаст его от root)
mkdir -p "$SCRIPT_DIR/.runtime"

echo ">>> Поднимаем smoke-стенд (события: $SMOKE_EVENT_SINK)"
# Готовность synaq проверяет conftest.py (/health с таймаутом), поэтому без --wait
"${COMPOSE[@]}" up --build -d

echo ">>> pytest $SCRIPT_DIR $*"
cd "$ROOT_DIR"
set +e
"$PYTHON" -m pytest "$SCRIPT_DIR" -p no:cacheprovider "$@"
rc=$?
set -e
if [[ $rc -ne 0 ]]; then
    echo ">>> Логи synaq (хвост):"
    "${COMPOSE[@]}" logs --no-color --tail=30 synaq 2>/dev/null || true
fi
exit $rc
