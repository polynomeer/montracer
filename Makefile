# Montracer 개발 경험 계약 (D06 §10~11).
# 아직 구현되지 않은 타깃은 실패 코드와 함께 안내만 출력한다. 구현 순서는 docs/plan/work-plan.md §5.0.

SHELL := /bin/bash

# 로컬 설정(.env)을 make 변수로 읽는다. 없으면 대상별로 bootstrap 안내.
-include .env
PROFILE ?= lite
SCENARIO ?= checkout

.DEFAULT_GOAL := help
.PHONY: help doctor bootstrap up down ps logs clean-data migrate migrate-status test-integration seed dev smoke test lint fmt test-contract test-isolation demo-reset docs

help: ## 사용 가능한 타깃 목록
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

doctor: ## runtime·port·memory·version 점검
	@scripts/dev/doctor.sh

bootstrap: ## toolchain 검증, .env 생성(기존 유지), Go·pnpm 의존성 설치
	@scripts/dev/doctor.sh || true
	@if [ -f .env ]; then echo ".env 이미 존재 — 유지"; else cp .env.example .env && echo ".env 생성"; fi
	go mod download
	pnpm install --frozen-lockfile

docs: ## docx 원본에서 docs/specs/*.md 재생성
	@python3 scripts/docs/convert_specs.py

COMPOSE_FILE_PATH = deploy/compose/compose.$(PROFILE).yaml
COMPOSE = docker compose --env-file deploy/compose/versions.env --env-file .env -f $(COMPOSE_FILE_PATH)

.env:
	@echo ".env 없음 — make bootstrap 을 먼저 실행하세요"; exit 1

up: .env ## 의존 서비스 기동 후 healthy까지 대기 (PROFILE=lite)
	@test -f $(COMPOSE_FILE_PATH) || { echo "미구현 profile: $(PROFILE) ($(COMPOSE_FILE_PATH) 없음)"; exit 1; }
	$(COMPOSE) up -d --wait --wait-timeout 180
	@scripts/dev/wait-collector.sh

down: .env ## 서비스만 종료 (데이터 볼륨은 유지)
	$(COMPOSE) down

ps: .env ## 서비스 상태
	$(COMPOSE) ps

logs: .env ## 서비스 로그 (SERVICE=kafka 처럼 지정 가능)
	$(COMPOSE) logs -f --tail=200 $(SERVICE)

clean-data: .env ## 로컬 데이터 볼륨 삭제 (확인 필요)
	@read -r -p "로컬 PostgreSQL·Kafka·ClickHouse 데이터를 모두 삭제합니다. 계속하려면 'delete' 입력: " ans; \
	  [ "$$ans" = "delete" ] || { echo "취소"; exit 1; }
	$(COMPOSE) down -v

# ClickHouse 계정 기본값 (compose 기본값과 같다, ADR 0018). .env에서 덮어쓸 수 있다.
CLICKHOUSE_DB ?= montracer
CLICKHOUSE_NATIVE_PORT ?= 19000
CLICKHOUSE_ADMIN_USER ?= montracer_admin
CLICKHOUSE_ADMIN_PASSWORD ?= local-dev-only-admin
CLICKHOUSE_QUERY_USER ?= montracer_query
CLICKHOUSE_QUERY_PASSWORD ?= local-dev-only
CLICKHOUSE_INGEST_USER ?= montracer_ingest
CLICKHOUSE_INGEST_PASSWORD ?= local-dev-only
CH_DSN = clickhouse://$(1):$(2)@localhost:$(CLICKHOUSE_NATIVE_PORT)/$(CLICKHOUSE_DB)
CH_ADMIN_DSN = $(call CH_DSN,$(CLICKHOUSE_ADMIN_USER),$(CLICKHOUSE_ADMIN_PASSWORD))

PG_ADMIN_DSN = postgres://$(POSTGRES_ADMIN_USER):$(POSTGRES_ADMIN_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable

migrate: .env ## PostgreSQL·ClickHouse migration 적용 (ADR 0016, 0018)
	@MONTRACER_MIGRATE_DSN='$(PG_ADMIN_DSN)' go run ./cmd/migrate up
	@$(COMPOSE) exec -T postgres sh -c 'psql -q -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -c "GRANT montracer_rw TO \"$$MONTRACER_APP_USER\""'
	@MONTRACER_MIGRATE_CH_DSN='$(CH_ADMIN_DSN)' go run ./cmd/migrate clickhouse up

migrate-status: .env ## migration 상태 (PostgreSQL, ClickHouse)
	@MONTRACER_MIGRATE_DSN='$(PG_ADMIN_DSN)' go run ./cmd/migrate postgres status
	@MONTRACER_MIGRATE_CH_DSN='$(CH_ADMIN_DSN)' go run ./cmd/migrate clickhouse status

seed: ## 2 tenant와 알려진 장애 fixture 적재 (SCENARIO=checkout), 재실행해도 logical 중복 없음
	@$(call todo,seed,tests/fixtures + 샘플 서비스)

dev: ## API·worker·UI 개발 모드 실행
	@$(call todo,dev,cmd/* 와 apps/web)

smoke: ## correlation·monitor·tenant 격리 smoke 시험
	@$(call todo,smoke,tests/e2e)

GO_PKGS = $(shell go list ./... 2>/dev/null)

test: ## 단위 테스트 (Go + JS workspace)
	@if [ -n "$(GO_PKGS)" ]; then go test -race ./...; else echo "Go 패키지 없음 — 건너뜀"; fi
	pnpm run test

test-integration: .env ## 통합 테스트 (make up 필요, 실행 중인 로컬 PostgreSQL 사용)
	@MONTRACER_TEST_PG_ADMIN_DSN='$(PG_ADMIN_DSN)' \
	MONTRACER_TEST_PG_APP_DSN='postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable' \
	MONTRACER_TEST_CH_ADMIN_DSN='$(CH_ADMIN_DSN)' \
	MONTRACER_TEST_CH_QUERY_DSN='$(call CH_DSN,$(CLICKHOUSE_QUERY_USER),$(CLICKHOUSE_QUERY_PASSWORD))' \
	MONTRACER_TEST_CH_INGEST_DSN='$(call CH_DSN,$(CLICKHOUSE_INGEST_USER),$(CLICKHOUSE_INGEST_PASSWORD))' \
	go test -race -count=1 -tags=integration ./...

lint: ## 정적 분석 (go vet, golangci-lint, JS workspace lint·typecheck)
	@if [ -n "$(GO_PKGS)" ]; then go vet ./...; else echo "Go 패키지 없음 — go vet 건너뜀"; fi
	@if [ -z "$(GO_PKGS)" ]; then :; elif command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint 없음 — CI에서 실행됨 (설치: brew install golangci-lint)"; fi
	pnpm run lint
	pnpm run typecheck

fmt: ## Go 코드 포맷
	gofmt -w $$(git ls-files '*.go')

test-contract: ## API·proto·UI fixture 일치 검사
	@$(call todo,test-contract,tests/contract)

test-isolation: ## query·stream·object·export cross-tenant 공격 시험
	@$(call todo,test-isolation,tests/isolation)

demo-reset: ## 명시 승인 후 demo 데이터만 삭제 (production endpoint 거절)
	@$(call todo,demo-reset,demo tenant 삭제 스크립트)

define todo
echo "미구현: make $(1) — $(2) 구현 필요 (docs/plan/work-plan.md §5.0)"; exit 1
endef
