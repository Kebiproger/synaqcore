#!/usr/bin/env bash
# Нагрузочный бенчмарк SynaqCore: heavy (ветка main / v1-python) против lite (ветка lite).
#
#   tests/bench/run.sh <heavy|lite> <variants|tracker|mixed>
#
# Что делает: поднимает стек из нужного worktree отдельным compose-проектом
# (synbench-<версия>, сеть synbench_net, все контейнеры в cpuset SUT_CPUS), заводит
# эксперимент, гоняет лестницу нагрузки k6 (docker, --network host, другие ядра),
# затем подтверждающие прогоны в колене и на 50% колена, проверяет целостность
# событий (строки в хранилище против принятых 2xx), пишет
# tests/bench/results/<версия>-<сценарий>-<дата>.json и сносит стек (down -v).
#
# Переменные окружения (все необязательны):
#   RATES          ступени RPS через пробел   (100 200 300 400 500 600 800 1000 1500 2000 3000 4000 6000 8000 12000)
#   DURATION       длительность замера, c     (30)
#   WARMUP         прогрев каждой ступени, c  (5)
#   PAUSE          пауза между ступенями, c   (10)
#   BENCH_PROFILE  vps2 (по умолчанию: стек на 0-1) | wide (стек на 0-7, лестница до 60000 RPS, MAX_VUS 8000)
#   SUT_CPUS       cpuset стека               (0-1  — эмуляция VPS 2 vCPU)
#   K6_CPUS        cpuset генератора нагрузки (8-15)
#   K6_IMAGE       образ k6                   (grafana/k6:0.54.0)
#   MAX_VUS        потолок VU k6              (4000)
#   LITE_EVENT_STORE  postgres (по умолчанию) | clickhouse  — хранилище событий lite
#   HEAVY_UVICORN_EXTRA  доп. аргументы uvicorn heavy, напр. "--no-access-log"
#   BENCH_BASE_URL берётся напрямую, иначе IP контейнера gateway в synbench_net
#                  (в обход docker-proxy, см. README)
#   KEEP_STACK=1   не сносить стек в конце;  SKIP_UP=1 — стек уже поднят этим же скриптом
#   LADDER_ONLY=1  без подтверждающих прогонов
#   SETTLE         пауза после старта стека, c (45)
#   DRAIN_TIMEOUT  макс. ожидание дренажа событий после ступени, c (120)
#   LITE_DIR / HEAVY_DIR  каталоги worktree (по умолчанию: родитель tests/bench и основной worktree)
set -euo pipefail

VERSION="${1:-}"; SCENARIO="${2:-}"
case "$VERSION" in heavy|lite) ;; *) echo "usage: $0 <heavy|lite> <variants|tracker|mixed>" >&2; exit 2;; esac
case "$SCENARIO" in variants|tracker|mixed) ;; *) echo "usage: $0 <heavy|lite> <variants|tracker|mixed>" >&2; exit 2;; esac

BENCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LITE_DIR="${LITE_DIR:-$(cd "$BENCH/../.." && pwd)}"
HEAVY_DIR="${HEAVY_DIR:-$(git -C "$LITE_DIR" worktree list --porcelain | awk 'NR==1{print $2}')}"
PROFILE="${BENCH_PROFILE:-vps2}"
case "$PROFILE" in
  vps2) DEF_RATES="100 200 300 400 500 600 800 1000 1500 2000 3000 4000 6000 8000 12000"; DEF_CPUS="0-1"; DEF_VUS=4000;;
  wide) DEF_RATES="1000 2000 4000 6000 8000 12000 16000 20000 25000 30000 40000 50000 60000"; DEF_CPUS="0-7"; DEF_VUS=8000;;
  *) echo "BENCH_PROFILE: vps2|wide" >&2; exit 2;;
esac
RATES="${RATES:-$DEF_RATES}"
DURATION="${DURATION:-30}"; WARMUP="${WARMUP:-5}"; PAUSE="${PAUSE:-10}"
SUT_CPUS="${SUT_CPUS:-$DEF_CPUS}"; K6_CPUS="${K6_CPUS:-8-15}"
K6_IMAGE="${K6_IMAGE:-grafana/k6:0.54.0}"; MAX_VUS="${MAX_VUS:-$DEF_VUS}"
DRAIN_TIMEOUT="${DRAIN_TIMEOUT:-120}"
EXPERIMENT="bench_exp"
GATEWAY_PORT=8080; ZITADEL_PORT=8081   # порты по умолчанию compose; публикуются только на 127.0.0.1

if [ "$VERSION" = lite ]; then
  DIR="$LITE_DIR"; STORE="${LITE_EVENT_STORE:-postgres}"
else
  DIR="$HEAVY_DIR"; STORE=clickhouse
fi
TAG="$VERSION"; [ "$VERSION" = lite ] && [ "$STORE" = clickhouse ] && TAG="lite-clickhouse"
[ "$PROFILE" = vps2 ] || TAG="$TAG-$PROFILE"
PROJECT="synbench-$VERSION"
WORK="$BENCH/.work/$VERSION"
STAMP="$(date +%Y%m%d-%H%M%S)"
RAW="$BENCH/results/raw/$TAG-$SCENARIO-$STAMP"
OUT="$BENCH/results/$TAG-$SCENARIO-$STAMP.json"
RUN_ID="b$(date +%s)"

export BENCH_WORK="$WORK" HEAVY_DIR LITE_DIR SUT_CPUS EXPERIMENT MAX_VUS
export HEAVY_UVICORN_EXTRA="${HEAVY_UVICORN_EXTRA:-}"
[ "$VERSION" = lite ] && export EVENT_STORE="$STORE"
PROFILE_ARGS=()
[ "$VERSION" = lite ] && [ "$STORE" = clickhouse ] && PROFILE_ARGS=(--profile scale)

log() { printf '[bench %s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "ОШИБКА: $*"; exit 1; }

dc() { (cd "$DIR" && docker compose -p "$PROJECT" "${PROFILE_ARGS[@]}" \
          -f docker-compose.yml -f "$BENCH/compose.$VERSION.yml" "$@"); }

SAMPLER_PID=""
CLEANED=0
cleanup() {
  [ "$CLEANED" = 1 ] && return 0
  CLEANED=1
  set +e +u
  [ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null
  # Контейнер k6 этого запуска (имя задаётся в run_step): убираем всегда, даже при KEEP_STACK=1.
  docker rm -f "synbench-k6-$$" >/dev/null 2>&1
  if [ "${KEEP_STACK:-0}" != 1 ]; then
    log "снос стека $PROJECT (down -v)"
    # Те же переменные, что нужны compose-файлам при старте (путь к worktree, cpuset, ...):
    # без них 'docker compose down' падает на пустом ${BENCH_WORK}/${LITE_DIR}.
    export BENCH_WORK="$WORK" HEAVY_DIR LITE_DIR SUT_CPUS EXPERIMENT MAX_VUS HEAVY_UVICORN_EXTRA
    [ "$VERSION" = lite ] && export EVENT_STORE="$STORE"
    dc down -v --remove-orphans >/dev/null 2>&1
  fi
  return 0
}
# INT/TERM превращаем в обычный выход, чтобы сработал trap EXIT (коды 130/143).
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- env-файлы: если нет, создаём из примеров со свежими случайными секретами -----------------
ensure_env() {
  python3 - "$1" <<'PY'
import re, secrets, string, sys, datetime
from pathlib import Path
root = Path(sys.argv[1])
infra, apps = root / "infra/.env.infra", root / "apps/.env.apps"
alnum = string.ascii_letters + string.digits
rnd = lambda n: "".join(secrets.choice(alnum) for _ in range(n))
def fill(example, target, known):
    out = []
    for line in example.read_text().splitlines():
        m = re.match(r"^([A-Z0-9_]+)=(.*)$", line)
        if m:
            k, v = m.groups()
            if k in known:
                v = known[k]
            elif k == "ZITADEL_MASTERKEY":
                v = rnd(32)
            elif k == "ZITADEL_FIRSTINSTANCE_ORG_HUMAN_PASSWORD":
                v = "Aa1!" + rnd(20)
            elif k == "ZITADEL_BOOTSTRAP_PAT_EXPIRATION":
                v = (datetime.datetime.utcnow() + datetime.timedelta(days=30)).strftime("%Y-%m-%dT00:00:00Z")
            elif re.search(r"(PASSWORD|SECRET|API_KEY)$", k) and (v == "" or v.startswith("change_me")):
                v = rnd(32)
            known[k] = v
            line = f"{k}={v}"
        out.append(line)
    target.write_text("\n".join(out) + "\n")
    target.chmod(0o600)
known = {}
if not infra.exists():
    fill(Path(str(infra) + ".example"), infra, known)
else:
    for l in infra.read_text().splitlines():
        m = re.match(r"^([A-Z0-9_]+)=(.*)$", l)
        if m: known[m.group(1)] = m.group(2)
if not apps.exists():
    fill(Path(str(apps) + ".example"), apps, {k: v for k, v in known.items() if "PASSWORD" in k or k.startswith("POSTGRES_")})
PY
}

# --- проверки перед стартом ---------------------------------------------------------------
preflight() {
  command -v jq >/dev/null && command -v curl >/dev/null || die "нужны jq и curl"
  [ -f "$DIR/docker-compose.yml" ] || die "нет $DIR/docker-compose.yml"
  ensure_env "$DIR"
  docker image inspect "$K6_IMAGE" >/dev/null 2>&1 || docker pull "$K6_IMAGE" >&2
  # Только один стек за раз: чужих synbench-* и контейнеров с теми же именами быть не должно.
  local other
  other="$(docker ps -a --filter 'label=com.docker.compose.project' --format '{{.Label "com.docker.compose.project"}}' \
           | sort -u | grep -E '^synbench-' | grep -vx "$PROJECT" || true)"
  [ -z "$other" ] || die "уже запущен другой бенч-стек: $other (остановите его)"
  local names clash
  names="$(dc config --format json | jq -r '.services[].container_name // empty')"
  for n in $names; do
    clash="$(docker ps -a --filter "name=^${n}$" --format '{{.Names}} {{.Label "com.docker.compose.project"}}' || true)"
    if [ -n "$clash" ] && [ "${clash#* }" != "$PROJECT" ]; then
      die "контейнер '$n' уже существует (проект '${clash#* }'): не трогаю чужое, остановите его вручную"
    fi
  done
  if [ "${SKIP_UP:-0}" != 1 ]; then
    for p in $GATEWAY_PORT $ZITADEL_PORT; do
      if ss -ltn "( sport = :$p )" | grep -q LISTEN; then die "порт 127.0.0.1:$p занят"; fi
    done
  fi
}

# --- nginx.conf без limit_req и access_log (оригинал не правим) -----------------------------
make_nginx_conf() {
  sed -E -e '/^[[:space:]]*limit_req(_status)?[[:space:]]/d' \
         -e 's/^server_tokens off;/server_tokens off;\naccess_log off;/' \
      "$DIR/nginx/nginx.conf" > "$WORK/nginx.conf"
  grep -q '^access_log off;' "$WORK/nginx.conf" || die "не удалось добавить access_log off"
  ! grep -Eq '^[[:space:]]*limit_req[[:space:]_]*(status)?[[:space:]]+(zone|[0-9])' "$WORK/nginx.conf" || die "limit_req остался"
}

bring_up() {
  rm -rf "$WORK"; mkdir -p "$WORK/zitadel/secrets" "$WORK/zitadel/public"
  chmod 700 "$WORK/zitadel/secrets"; chmod 755 "$WORK/zitadel/public"
  make_nginx_conf
  log "проверка конфигурации (docker compose config)"
  dc config -q
  log "сборка и старт стека $PROJECT (cpuset $SUT_CPUS)"
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  dc up -d --build >&2
  log "ожидание Zitadel-bootstrap и gateway"
  local t=0
  until [ -s "$WORK/zitadel/public/oidc.json" ] && [ -s "$WORK/zitadel/secrets/automation.json" ]; do
    sleep 3; t=$((t+3)); [ $t -lt 900 ] || die "zitadel-init не создал oidc.json/automation.json"
  done
  t=0
  until curl -fsS -o /dev/null "http://127.0.0.1:$GATEWAY_PORT/health"; do
    sleep 2; t=$((t+2)); [ $t -lt 300 ] || die "gateway не отвечает на /health"
  done
}

seed_experiment() {
  local token code
  token="$(AUTOMATION_FILE="$WORK/zitadel/secrets/automation.json" OIDC_CONFIG_FILE="$WORK/zitadel/public/oidc.json" \
           "$DIR/scripts/get_token.sh")" || die "не удалось получить токен"
  # Токен передаётся через stdin curl, а не аргументом (не светится в ps)
  code="$(printf 'Authorization: Bearer %s\n' "$token" | curl -sS -o "$WORK/seed.out" -w '%{http_code}' \
          -H @- -H 'Content-Type: application/json' -X POST "http://127.0.0.1:$GATEWAY_PORT/api/v1/experiments/" \
          -d "{\"name\":\"$EXPERIMENT\",\"variants\":{\"control\":50,\"treatment\":50},\"is_active\":true}")"
  unset token
  case "$code" in 201|200) log "эксперимент $EXPERIMENT создан";; 400) log "эксперимент уже существует";;
    *) die "создание эксперимента: HTTP $code: $(head -c 300 "$WORK/seed.out")";; esac
}

# --- хранилище событий -----------------------------------------------------------------------
CH_TABLE=""
count_rows() {
  if [ "$VERSION" = heavy ] || [ "$STORE" = clickhouse ]; then
    if [ -z "$CH_TABLE" ]; then
      local db
      db="$(docker exec -i sb-clickhouse sh -c 'clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD"' \
            <<< "SELECT database FROM system.tables WHERE name='tracking_events' AND engine <> 'Kafka' LIMIT 1")"
      CH_TABLE="${db}.tracking_events"
    fi
    docker exec -i sb-clickhouse sh -c 'clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD"' \
      <<< "SELECT count() FROM $CH_TABLE WHERE event_name='bench_conversion'"
  else
    docker exec -i sb-postgres-app sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tA' \
      <<< "SELECT count(*) FROM events.tracking_events WHERE event_name='bench_conversion'"
  fi
}
kafka_lag() {
  if [ "$VERSION" = heavy ]; then
    docker exec sb-redpanda rpk group describe clickhouse_tracking_cg 2>/dev/null | awk '/TOTAL-LAG/{print $2}' | head -1
  else
    echo 0
  fi
}

# Ждёт, пока число строк (минус исходное) достигнет accepted; пишет <label>.drain.json.
# Если за DRAIN_TIMEOUT не достигло: выходим, когда строки не растут и лаг Kafka = 0 (потеря).
wait_drain() {
  local label="$1" accepted="$2" t0 now rows last=-1 same=0 backlog first_rows lag t_last_change
  t0="$(date +%s.%N)"; t_last_change="$t0"
  first_rows=$(( $(count_rows) - ROWS_BEFORE ))
  backlog=$(( accepted - first_rows ))
  while :; do
    rows=$(( $(count_rows) - ROWS_BEFORE )); now="$(date +%s.%N)"
    [ "$rows" != "$last" ] && { t_last_change="$now"; same=0; } || same=$((same+1))
    last="$rows"
    if [ "$rows" -ge "$accepted" ]; then break; fi
    lag="$(kafka_lag)"; lag="${lag:-0}"
    if [ "$same" -ge 8 ] && [ "$lag" = 0 ]; then break; fi
    if awk -v n="$now" -v s="$t0" -v m="$DRAIN_TIMEOUT" 'BEGIN{exit !(n-s>m)}'; then break; fi
    sleep 1
  done
  local drain_s
  drain_s="$(awk -v a="$t_last_change" -v b="$t0" 'BEGIN{printf "%.2f", a-b}')"
  jq -n --argjson acc "$accepted" --argjson rows "$rows" --argjson backlog "$backlog" --argjson d "$drain_s" \
     '{accepted_cum:$acc, rows_cum:$rows, backlog_at_k6_end:$backlog, drain_s:($d*10|round/10), drained:($rows>=$acc)}' \
     > "$RAW/$label.drain.json"
}

# --- запуск одной ступени -----------------------------------------------------------------
sample_stats() {
  local out="$1" names
  names="$(docker ps --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Names}}')"
  while :; do
    # shellcheck disable=SC2086
    docker stats --no-stream --format '{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}' $names "synbench-k6-$$" 2>/dev/null \
      | awk -v ts="$(date +%s)" '{print ts "\t" $0}' >> "$out" || true
    sleep 1
  done
}

CUM_ACCEPTED=0
run_step() {   # run_step <label> <kind> <rate>
  local label="$1" kind="$2" rate="$3"
  log "ступень $label: $rate RPS, прогрев ${WARMUP}s + замер ${DURATION}s"
  jq -n --arg l "$label" --arg k "$kind" --argjson r "$rate" --argjson d "$DURATION" \
     '{label:$l,kind:$k,rate:$r,duration_s:$d}' > "$RAW/$label.meta.json"
  : > "$RAW/$label.stats.tsv"
  sample_stats "$RAW/$label.stats.tsv" & SAMPLER_PID=$!
  local rc=0
  docker run --rm --name "synbench-k6-$$" --network host --cpuset-cpus "$K6_CPUS" \
    --ulimit nofile=1048576:1048576 --user "$(id -u):$(id -g)" \
    -v "$BENCH/k6:/bench:ro" -v "$RAW:/out" \
    -e BASE_URL="$BASE_URL" -e EXPERIMENT -e RATE="$rate" -e DURATION="${DURATION}s" -e WARMUP="${WARMUP}s" \
    -e RUN_ID="$RUN_ID-$label" -e MAX_VUS \
    "$K6_IMAGE" run --quiet --summary-export "/out/$label.json" "/bench/$SCENARIO.js" \
    > "$RAW/$label.k6.log" 2>&1 || rc=$?
  kill "$SAMPLER_PID" 2>/dev/null || true; wait "$SAMPLER_PID" 2>/dev/null || true; SAMPLER_PID=""
  # rc=99 — просто сработали thresholds (ступень не прошла), это ожидаемо
  if [ "$rc" != 0 ] && [ "$rc" != 99 ]; then log "k6 завершился с кодом $rc, см. $RAW/$label.k6.log"; fi
  if [ ! -s "$RAW/$label.json" ]; then log "нет summary для $label"; return 1; fi
  if [ "$SCENARIO" != variants ]; then
    local acc; acc="$(python3 "$BENCH/report.py" accepted "$RAW/$label.json" "$rate" "$DURATION")"
    CUM_ACCEPTED=$((CUM_ACCEPTED + acc))
    wait_drain "$label" "$CUM_ACCEPTED"
  fi
  return 0
}

verdict_of() { python3 "$BENCH/report.py" verdict "$RAW/$1.json" "$2" "$DURATION"; }

# =================================== main ===================================================
mkdir -p "$RAW" "$BENCH/results"
preflight
[ "${SKIP_UP:-0}" = 1 ] || bring_up
[ -d "$WORK" ] || die "нет $WORK (SKIP_UP=1 без поднятого стека?)"
[ "${SKIP_UP:-0}" = 1 ] || seed_experiment
if [ -n "${BENCH_BASE_URL:-}" ]; then
  BASE_URL="$BENCH_BASE_URL"
else
  gip="$(docker inspect -f '{{(index .NetworkSettings.Networks "synbench_net").IPAddress}}' sb-gateway)"
  [ -n "$gip" ] || die "не найден IP gateway"
  BASE_URL="http://$gip:80"
fi
log "BASE_URL=$BASE_URL, версия=$TAG, сценарий=$SCENARIO"

# Zitadel и Postgres после старта ещё некоторое время заметно грузят CPU: даём стеку успокоиться
if [ "${SKIP_UP:-0}" != 1 ]; then log "стабилизация стека ${SETTLE:-45}s"; sleep "${SETTLE:-45}"; fi

ROWS_BEFORE=0
[ "$SCENARIO" = variants ] || ROWS_BEFORE="$(count_rows)"

jq -n --arg v "$TAG" --arg s "$SCENARIO" --arg c "$(git -C "$DIR" rev-parse --short HEAD)" \
      --arg b "$(git -C "$DIR" rev-parse --abbrev-ref HEAD)" --arg sut "$SUT_CPUS" --arg k6c "$K6_CPUS" \
      --arg img "$K6_IMAGE" --arg base "$BASE_URL" --arg rates "$RATES" --arg date "$(date -Is)" \
      --argjson dur "$DURATION" --argjson wu "$WARMUP" --arg load "$(cut -d' ' -f1-3 /proc/loadavg)" \
      '{version:$v,scenario:$s,commit:$c,branch:$b,sut_cpus:$sut,k6_cpus:$k6c,k6_image:$img,base_url:$base,
        rates:$rates,duration_s:$dur,warmup_s:$wu,host_loadavg_at_start:$load,date:$date}' > "$RAW/run-info.json"

# 1) предварительный прогрев всего пути (пулы соединений, JIT, кэши)
run_step prewarm prewarm 100 || true

# 2) лестница; останавливаемся на первой "насыщенной" ступени
for r in $RATES; do
  run_step "l$r" ladder "$r" || { log "ступень $r не дала данных, стоп"; break; }
  v="$(verdict_of "l$r" "$r")"
  log "ступень $r: $v"
  case "$v" in SATURATED*) log "насыщение, лестница остановлена"; break;; esac
  sleep "$PAUSE"
done

# 3) подтверждающие прогоны: колено ещё раз (если не прошло: шаг вниз по лестнице),
#    затем 50% подтверждённого колена дважды
KNEE_L="$(python3 "$BENCH/report.py" knee "$RAW")"
log "колено лестницы: ${KNEE_L:-нет}"
KNEE_C=""
if [ -n "$KNEE_L" ] && [ "${LADDER_ONLY:-0}" != 1 ]; then
  cands=()
  for r in $RATES; do [ "$r" -le "$KNEE_L" ] && cands=("$r" "${cands[@]}"); done
  for K in "${cands[@]}"; do
    sleep "$PAUSE"
    run_step "k${K}b" knee "$K" || continue
    j="$(python3 "$BENCH/report.py" judge "$RAW/k${K}b.json" "$K" "$DURATION")"
    log "повтор на $K RPS: $j"
    if [ "$j" = OK ]; then KNEE_C="$K"; break; fi
  done
  if [ -n "$KNEE_C" ]; then
    HALF=$(( KNEE_C / 2 ))
    sleep "$PAUSE"; run_step "h${HALF}a" half "$HALF" || true
    sleep "$PAUSE"; run_step "h${HALF}b" half "$HALF" || true
  fi
fi
jq -n --arg l "$KNEE_L" --arg c "$KNEE_C" '{ladder_knee:($l|tonumber? // null), confirmed_knee:($c|tonumber? // null)}' > "$RAW/knee.json"

# 4) итоговая целостность
if [ "$SCENARIO" != variants ]; then
  ROWS_NOW=$(( $(count_rows) - ROWS_BEFORE ))
  jq -n --argjson rows "$ROWS_NOW" --argjson lag "$(kafka_lag | awk '{print ($1==""?0:$1)}')" \
        --arg store "$STORE" '{store:$store, rows_delta:$rows, kafka_lag_at_end:$lag}' > "$RAW/integrity.json"
fi

python3 "$BENCH/report.py" aggregate "$RAW" "$OUT" | tee "$RAW/table.md"
log "результат: $OUT (сырые данные: $RAW)"
