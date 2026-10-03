# Montracer 개발 경험 계약 (D06 §10~11).
# 아직 구현되지 않은 타깃은 실패 코드와 함께 안내만 출력한다. 구현 순서는 docs/plan/work-plan.md §5.0.

SHELL := /bin/bash
PROFILE ?= lite
SCENARIO ?= checkout

.DEFAULT_GOAL := help
.PHONY: help doctor bootstrap up down migrate seed dev smoke test test-contract test-isolation demo-reset docs

help: ## 사용 가능한 타깃 목록
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

doctor: ## runtime·port·memory·version 점검
	@scripts/dev/doctor.sh

bootstrap: ## toolchain 검증 후 .env.example → .env 복사 (기존 .env 유지)
	@scripts/dev/doctor.sh || true
	@if [ -f .env ]; then echo ".env 이미 존재 — 유지"; else cp .env.example .env && echo ".env 생성"; fi

docs: ## docx 원본에서 docs/specs/*.md 재생성
	@python3 scripts/docs/convert_specs.py

up: ## 의존 서비스 기동 (PROFILE=lite|full)
	@$(call todo,up,deploy/compose/$(PROFILE) compose 파일)

down: ## 서비스만 종료 (데이터는 삭제하지 않음)
	@$(call todo,down,deploy/compose)

migrate: ## PostgreSQL·ClickHouse migration 적용
	@$(call todo,migrate,migrations/)

seed: ## 2 tenant와 알려진 장애 fixture 적재 (SCENARIO=checkout), 재실행해도 logical 중복 없음
	@$(call todo,seed,tests/fixtures + 샘플 서비스)

dev: ## API·worker·UI 개발 모드 실행
	@$(call todo,dev,cmd/* 와 apps/web)

smoke: ## correlation·monitor·tenant 격리 smoke 시험
	@$(call todo,smoke,tests/e2e)

test: ## 단위 테스트
	@$(call todo,test,Go module·pnpm workspace)

test-contract: ## API·proto·UI fixture 일치 검사
	@$(call todo,test-contract,tests/contract)

test-isolation: ## query·stream·object·export cross-tenant 공격 시험
	@$(call todo,test-isolation,tests/isolation)

demo-reset: ## 명시 승인 후 demo 데이터만 삭제 (production endpoint 거절)
	@$(call todo,demo-reset,demo tenant 삭제 스크립트)

define todo
echo "미구현: make $(1) — $(2) 구현 필요 (docs/plan/work-plan.md §5.0)"; exit 1
endef
