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
.PHONY: help up up-scale down token demo test lint go-test smoke smoke-ch zitadel-reset check-masterkey check-db-passwords check-clickhouse-password

help: ## Показать список команд
	@echo "Использование: make <цель>"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-22s %s\n", $$1, $$2}'
	@echo
	@echo "Пример: eval \"\$$(make -s token)\" && make demo DEMO_ARGS=\"--users 1000 --seed 42\""

up: check-masterkey check-db-passwords ## Поднять весь стек, события в Postgres (docker compose up -d --build)
	docker compose up -d --build

# Профиль scale: ClickHouse как хранилище событий (EVENT_STORE=clickhouse).
# Переменная shell перекрывает значение из infra/.env.infra.
up-scale: check-masterkey check-db-passwords check-clickhouse-password ## Поднять стек с ClickHouse (профиль scale)
	EVENT_STORE=clickhouse docker compose --profile scale up -d --build

# Мастер-ключ Zitadel нельзя сменить после первой инициализации, поэтому не даём
# стартовать с образцовым ключом из документации или ключом неверной длины.
# Сам ключ никогда не печатается: grep -q и подсчёт длины в подоболочке.
# Значение разбирается как в env-файле docker compose: CRLF, отступ, "export ",
# пробелы вокруг "="; в кавычках берётся содержимое (# — часть значения, хвост
# после закрывающей кавычки игнорируется); без кавычек отрезается " #комментарий"
# и хвостовые пробелы. Используется последняя строка ZITADEL_MASTERKEY=.
ENV_INFRA := infra/.env.infra
# Значение переменной $(1) из $(ENV_INFRA) по правилам выше (последняя строка);
# на выход идёт только значение — вызывать лишь внутри подоболочки с read.
env_value = tr -d '\r' < $(ENV_INFRA) | \
	sed -n 's/^[[:space:]]*\(export[[:space:]][[:space:]]*\)\{0,1\}$(1)[[:space:]]*=[[:space:]]*//p' | \
	tail -n 1 | \
	sed -e 's/^"\([^"]*\)".*$$/\1/' -e t -e "s/^'\([^']*\)'.*$$/\1/" -e t \
		-e 's/[[:space:]]\#.*$$//' -e 's/[[:space:]]*$$//'
SAMPLE_MASTERKEY := MasterkeyNeedsToHave32Characters

check-masterkey: ## Проверить ZITADEL_MASTERKEY в infra/.env.infra (не образец, ровно 32 символа)
	@if [ ! -f $(ENV_INFRA) ]; then \
		echo "Нет $(ENV_INFRA): cp infra/.env.infra.example $(ENV_INFRA) и заполните" >&2; exit 1; \
	fi
	@if grep -q '$(SAMPLE_MASTERKEY)' $(ENV_INFRA); then \
		echo "ZITADEL_MASTERKEY в $(ENV_INFRA) — образцовый ключ из документации." >&2; \
		echo "Сгенерируйте свой: tr -dc A-Za-z0-9 </dev/urandom | head -c 32" >&2; exit 1; \
	fi
	@status=$$(tr -d '\r' < $(ENV_INFRA) | \
		sed -n 's/^[[:space:]]*\(export[[:space:]][[:space:]]*\)\{0,1\}ZITADEL_MASTERKEY[[:space:]]*=[[:space:]]*//p' | \
		tail -n 1 | \
		sed -e 's/^"\([^"]*\)".*$$/\1/' -e t -e "s/^'\([^']*\)'.*$$/\1/" -e t \
			-e 's/[[:space:]]#.*$$//' -e 's/[[:space:]]*$$//' | \
		{ IFS= read -r key; [ $${#key} -eq 32 ] && echo OK || echo FAIL; }); \
	if [ "$$status" != OK ]; then \
		echo "ZITADEL_MASTERKEY в $(ENV_INFRA) должен быть ровно 32 символа." >&2; \
		echo "Проверьте, что не скопирован символ % в конце вывода tr (zsh) и нет лишних символов." >&2; \
		echo "Сгенерируйте: tr -dc A-Za-z0-9 </dev/urandom | head -c 32" >&2; exit 1; \
	fi

# Пароли ролей synaq_app / synaq_analytics (создаёт postgres-app-init): те же правила,
# что в infra/postgres-app/init-roles.sh, но ошибка видна до старта стека. Значения
# не печатаются: только в переменных подоболочки.
check-db-passwords: ## Проверить SYNAQ_DB_PASSWORD / ANALYTICS_DB_PASSWORD в infra/.env.infra (>=16 символов, разные)
	@if [ ! -f $(ENV_INFRA) ]; then \
		echo "Нет $(ENV_INFRA): cp infra/.env.infra.example $(ENV_INFRA) и заполните" >&2; exit 1; \
	fi
	@a=$$($(call env_value,SYNAQ_DB_PASSWORD)); b=$$($(call env_value,ANALYTICS_DB_PASSWORD)); \
	if [ $${#a} -eq 0 ] || [ $${#b} -eq 0 ]; then \
		echo "В $(ENV_INFRA) должны быть заданы SYNAQ_DB_PASSWORD и ANALYTICS_DB_PASSWORD." >&2; \
		echo "Сгенерируйте каждый: tr -dc A-Za-z0-9 </dev/urandom | head -c 32" >&2; exit 1; \
	fi; \
	if [ $${#a} -lt 16 ] || [ $${#b} -lt 16 ]; then \
		echo "SYNAQ_DB_PASSWORD и ANALYTICS_DB_PASSWORD в $(ENV_INFRA) — не короче 16 символов." >&2; exit 1; \
	fi; \
	if [ "$$a" = "$$b" ]; then \
		echo "SYNAQ_DB_PASSWORD и ANALYTICS_DB_PASSWORD в $(ENV_INFRA) должны различаться." >&2; exit 1; \
	fi

# compose больше не требует CLICKHOUSE_PASSWORD (иначе ломался бы стек без профиля scale)
check-clickhouse-password: ## Проверить, что CLICKHOUSE_PASSWORD в infra/.env.infra не пуст (для up-scale)
	@if [ ! -f $(ENV_INFRA) ]; then \
		echo "Нет $(ENV_INFRA): cp infra/.env.infra.example $(ENV_INFRA) и заполните" >&2; exit 1; \
	fi
	@c=$$($(call env_value,CLICKHOUSE_PASSWORD)); \
	if [ $${#c} -eq 0 ]; then \
		echo "CLICKHOUSE_PASSWORD в $(ENV_INFRA) пуст: для профиля scale он обязателен." >&2; exit 1; \
	fi

# --profile scale: иначе ClickHouse (профиль scale) после up-scale остался бы запущенным
down: ## Остановить стек, включая ClickHouse профиля scale (docker compose --profile scale down)
	docker compose --profile scale down

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

go-test: ## Тесты Go (сервис synaq и sdk/go)
	cd apps/synaq && go test -race ./...
	cd sdk/go && go test -race ./...

smoke: ## Smoke-тесты HTTP-API synaq в отдельном docker-стенде (tests/contract/run.sh)
	PYTHON=$(PYTHON) tests/contract/run.sh -q

smoke-ch: ## То же на ClickHouse как хранилище событий (SMOKE_EVENT_SINK=clickhouse)
	SMOKE_EVENT_SINK=clickhouse PYTHON=$(PYTHON) tests/contract/run.sh -q

# Lite держит Zitadel в отдельном томе postgres-zitadel: после переезда со старого
# checkout (Python-версия, тег v1-python) Zitadel создаёт новый инстанс (новый
# project_id), а старые oidc.json / automation.json указывают на прежний.
# Запускать при ОСТАНОВЛЕННОМ стеке и до первого `make up` на новых томах: свежий
# bootstrap.pat пишет только первый setup, после удаления он не появится снова.
# Тома не трогаем — только подсказка.
zitadel-reset: ## Удалить сгенерированные OIDC-файлы Zitadel (public/*.json, secrets/*), с подтверждением
	@printf "Удалить infra/zitadel/public/*.json и содержимое infra/zitadel/secrets/ (кроме .gitkeep)? [y/N] "; \
	read ans; \
	case "$$ans" in \
		y|Y|yes|YES) ;; \
		*) echo "Отменено"; exit 0 ;; \
	esac; \
	rm -f infra/zitadel/public/*.json; \
	find infra/zitadel/secrets -mindepth 1 ! -name .gitkeep -exec rm -rf {} +; \
	echo "Готово. Если нужен полностью чистый Zitadel (новый инстанс), остановите стек"; \
	echo "и удалите тома вручную: docker compose down -v  (УДАЛИТ ВСЕ ДАННЫЕ стека), затем make up"
