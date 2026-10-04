# Montracer

Java·Kubernetes 조직을 위한 B2B 관측(APM) 플랫폼. 서비스 상태 → 요청(trace) 원인 조사 → JVM 진단 → 사용자 경험을 하나의 맥락으로 연결한다.

**현재 단계: Phase 0 (레포 부트스트랩) 마무리.** 골격·Go module·pnpm workspace·로컬 lite stack·CI가 있고 애플리케이션 코드는 아직 없다. ADR 0001~0003·0013 승인됨. 남은 Phase 0 작업은 [작업계획서](docs/plan/work-plan.md) §5.0.

## 문서 지도

- 설계 명세(권위): `docs/specs/D01~D06-*.md` — 색인과 권위 범위는 @docs/specs/README.md
  - 기능 범위·F ID → D01 / 저장·API 의미 → D02 / 계측·진단 → D03 / 보안·PII·운영·과금 → D04 / UI → D05 / 일정·테스트·레포 규약 → D06
- 작업계획서: `docs/plan/work-plan.md`, 기능 상태: `docs/plan/requirements-registry.md`
- 결정 기록: `docs/adr/` (등록부 README), 운영 절차: `docs/runbooks/`
- `docs/specs/*.md`는 docx 원본(`docs/specs/original/`)의 파생본이다. **직접 수정하지 않는다.** 원본 개정 후 `python3 scripts/docs/convert_specs.py`로 재생성.

명세는 크다(D02 ≈ 800줄). 통째로 읽지 말고 `grep -n "^## " docs/specs/D02-system-data-api.md`로 절을 찾은 뒤 필요한 절만 읽는다. 참조 표기 `D02 §05` = D02의 "## 05 ..." 절.

## 변경 불가 계약 (위반하는 코드는 작성하지 않는다)

1. tenant_id는 **인증 principal에서만** 도출. payload·`X-Tenant-ID`는 신뢰하지 않는다.
2. 수집 ACK는 **Kafka durable append 이후**에만 반환. 전체 경로는 at-least-once, "exactly-once"라고 쓰지 않는다.
3. **PII 제거는 최초 영속 저장 전.** redaction 실패 원문은 로그·quarantine·debug 어디에도 남기지 않는다.
4. 시간·tenant·권한 scope·expires_at·삭제 tombstone은 query의 **mandatory predicate**. 사용자 문자열을 SQL에 이어 붙이지 않는다(AST + parameter binding).
5. SLO·오류율은 **비샘플링 SDK metric**에서 계산. percentile을 평균하지 않는다(histogram bucket 병합).
6. missing / sampled / stale / partial / NO_DATA / EVALUATION_ERROR를 0이나 정상값으로 치환하지 않는다.
7. 문서 간 불일치는 임의로 고르지 않는다. 권위 문서 수정 + ADR + 계약 테스트로 해결하고, 사용자에게 먼저 알린다.

## 기술 스택 (ADR 0001~0003, 0013)

Go(ingress·API·worker) · OTel SDK/Collector · Kafka · ClickHouse(trace·log·metric) · PostgreSQL(제어, RLS + outbox) · React + TypeScript · Kubernetes/Helm. 레포 구조는 D06 §10~11의 모노레포. 각 최상위 디렉터리 README에 책임과 명세 절이 있다.

- `cmd/<service>/` 서비스 진입점(README에 소유 데이터·장애 동작) · `internal/{authz,telemetry,query,pipeline}` 공유 Go 패키지
- `api/{openapi,proto}` 계약 원천 · `migrations/{postgres,clickhouse}` · `deploy/{compose,helm}` · `infra/`
- `apps/web` UI · `packages/{design-tokens,ui,query-schema}` · `agents/` · `sdk/browser` · `integrations/packs`
- `tests/{fixtures,contract,isolation,e2e,load}` 교차 컴포넌트 시험

- Go 단일 module `github.com/polynomeer/montracer` (Go 1.26), JS는 pnpm workspace(`apps/*`, `packages/*`, `sdk/*`, Node 24 LTS, pnpm 11).
- 버전·이미지는 ADR 0013과 `deploy/compose/versions.env`(digest)가 단일 원천. 바꿀 때 ADR 0013을 같은 PR에서 갱신한다.

## 코딩 규약 (D06 §10)

- Go: gofmt + 정적 분석, context timeout·취소 전파, `fmt.Errorf("...: %w", err)` wrapping. tenant context는 함수 인자로 명시, 전역 mutable tenant 상태 금지. tenant 없는 repository method 금지.
- TypeScript: strict mode. 서버 원본 데이터를 전역 store에 복사하지 않는다. log·stack·SQL은 text-only 렌더링.
- 숫자 timestamp·duration은 단위를 이름에 붙인다 (`duration_ms`, `event_time_ns`). 외부 입력은 길이·범위 검증.
- 로그는 구조화, secret·payload·인증 header 금지. 재시도는 멱등성·backoff·총 시간 제한 명시.
- raw SQL은 repository / query planner에만. 테스트 fixture에 실제 고객 데이터·계정·secret 금지.
- 이미지·의존성은 digest/lockfile로 고정, `latest` 금지.

## 작업 방식

- main은 보호 브랜치다. 모든 변경은 브랜치(`feat/…`, `fix/…`, `docs/…`) → PR → 필수 CI 4개 통과 → merge. main에 직접 push하지 않는다.
- 커밋은 작업 단위로 나눈다. 메시지는 한국어 본문 + Conventional Commits 접두어(`feat:`, `fix:`, `docs:`, `chore:`, `test:`, `refactor:`).
- PR/브랜치에는 F ID와 Epic, schema 변경, tenant 영향, retention 영향, rollout plan을 적는다.
- 기능 완료 = 공통 DoD(D06 §02): 코드 + API + migration + 문서 + unit/통합/negative test + metric + runbook + rollback. flag off 경로도 시험.
- 범위·수치·기술 선택을 바꾸면 `docs/adr/`에 ADR을 쓴다 (`/adr` 스킬). 승인 이력을 임의로 만들지 않는다.
- 기능 상태가 바뀌면 `docs/plan/requirements-registry.md`를 함께 갱신한다.
- 기능 작업 시작 전 `/spec-context <F ID 또는 주제>`로 관련 명세 절을 모은다. 변경 후에는 `spec-reviewer` 에이전트로 계약 위반을 점검할 수 있다.

## 명령 (구현 후 제공될 개발 경험 계약, D06 §10~11)

`make doctor` · `make bootstrap` · `make up PROFILE=lite` · `make migrate` · `make seed SCENARIO=checkout` · `make dev` · `make smoke` · `make test-contract` · `make test-isolation` · `make down`
동작: `help`, `doctor`, `bootstrap`, `docs`, `up`/`down`/`ps`/`logs`/`clean-data`, `migrate`/`migrate-status`(PostgreSQL), `test`, `test-integration`(`-tags=integration`, 실행 중인 로컬 PG 사용), `lint`, `fmt`. 미구현(안내 후 실패): `seed`, `dev`, `smoke`, `test-contract`, `test-isolation`, `demo-reset`.
- 제어 DB 접근은 `internal/controldb`에서만 한다. tenant 범위 작업은 반드시 `WithTenant`를 거친다 (ADR 0016). 새 테이블은 migration에서 `ENABLE`+`FORCE RLS`, `montracer_rw`에 최소 권한 GRANT, 통합 테스트로 격리를 검증한다.
로컬 stack 포트는 1xxxx 대역(PG 15432, Kafka 19192, CH 18123/19000, OTLP 14317/14318) — `deploy/compose/README.md`.
