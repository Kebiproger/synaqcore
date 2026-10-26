#!/usr/bin/env bash
# Полный прогон: heavy и lite × variants/tracker/mixed, строго по одному стеку за раз.
#   tests/bench/run-all.sh            (параметры run.sh передаются через окружение)
set -uo pipefail
BENCH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for v in heavy lite; do
  for s in variants tracker mixed; do
    echo "=== $v $s $(date -Is)"
    "$BENCH/run.sh" "$v" "$s" || echo "!!! $v $s завершился с ошибкой ($?)"
  done
done
