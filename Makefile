# SynaqCore — частые команды. Справка: make (или make help)
#
# Скрипты в venv/bin имеют сломанный shebang, поэтому инструменты venv
# запускаются через `python -m`.

VENV_PY := ./venv/bin/python
# venv предпочтительнее, если он есть; иначе системный python3
# (для тестов/линтера в нём должны быть установлены pytest/ruff)
PYTHON  := $(shell [ -x $(VENV_PY) ] && echo $(VENV_PY) || echo python3)

# Дополнительные аргументы демо: make demo DEMO_ARGS="--users 1000 --seed 42"
DEMO_ARGS ?=

.DEFAULT_GOAL := help
.PHONY: help up down token demo test lint go-test ch-reset

help: ## Показать список команд
	@echo "Использование: make <цель>"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'
	@echo
	@echo "Пример: eval \"\$$(make -s token)\" && make demo DEMO_ARGS=\"--users 1000 --seed 42\""

up: ## Поднять весь стек (docker compose up -d --build)
	docker compose up -d --build

down: ## Остановить стек (docker compose down)
	docker compose down

token: ## Напечатать export SYNAQ_TOKEN=... (только в pipe/eval, см. пример ниже)
	@if [ -t 1 ]; then \
		echo "Токен не печатается в терминал. Используйте: eval \"\$$(make -s token)\"" >&2; \
		exit 1; \
	fi
	@TOKEN="$$(scripts/get_token.sh)" || exit 1; \
	case "$$TOKEN" in \
		''|*[!A-Za-z0-9._-]*) echo "Токен пустой или содержит недопустимые символы — не печатаю" >&2; exit 1 ;; \
	esac; \
	printf "export SYNAQ_TOKEN='%s'\n" "$$TOKEN"

demo: ## Запустить симулятор A/B-теста (токен из SYNAQ_TOKEN или scripts/get_token.sh)
	@TOKEN="$${SYNAQ_TOKEN:-}"; \
	if [ -z "$$TOKEN" ]; then \
		echo "SYNAQ_TOKEN не задан — получаю через scripts/get_token.sh"; \
		TOKEN="$$(scripts/get_token.sh)" || exit 1; \
	fi; \
	SYNAQ_TOKEN="$$TOKEN" $(PYTHON) sdk/demo_simulator.py $(DEMO_ARGS)

test: ## Юнит-тесты (pytest)
	$(PYTHON) -m pytest tests/ -q

lint: ## Линтер (ruff)
	$(PYTHON) -m ruff check apps tests sdk

go-test: ## Тесты Go (трекер и sdk/go)
	cd apps/track_api_go && go test -race ./...
	cd sdk/go && go test -race ./...

# Клиент ClickHouse внутри контейнера; логин и пароль берутся из окружения контейнера
# (infra/.env.infra), раскрываются его shell и не попадают в команду на хосте.
CH_CLIENT := docker compose exec -T clickhouse sh -c 'exec clickhouse-client --user "$${CLICKHOUSE_USER:-default}" --password "$${CLICKHOUSE_PASSWORD:-}" --multiquery'

ch-reset: ## Пересоздать Kafka-очередь и MV ClickHouse (данные tracking_events не трогаются)
	@# Сначала MV (читает из очереди), затем очередь. Таблицу tracking_events не удаляем:
	@# MV пишет в неё через TO, и DROP VIEW целевую таблицу не затрагивает.
	echo "DROP VIEW IF EXISTS tracking_events_mv; DROP TABLE IF EXISTS tracking_events_queue;" | $(CH_CLIENT)
	$(CH_CLIENT) < infra/clickhouse/init/02-kafka.sql
