# ADR 0016: 제어 DB(PostgreSQL) 접근 방식과 migration 도구

- 상태: 승인
- Owner: Backend
- 승인자: polynomeer
- 날짜: 2026-10-04 (제안) / 2026-10-04 (결정)
- 관련: F07, E01 · D02 §11, §14 · D04 §01 · D06 §07 · ADR 0013, 0015

## 배경

D02 §11은 다음을 요구한다.

- 모든 tenant 테이블에 `ENABLE` + `FORCE ROW LEVEL SECURITY`를 건다.
- 요청마다 `SET LOCAL app.tenant_id`를 설정한다.
- 앱 role은 owner, superuser, BYPASSRLS 권한을 갖지 않는다.
- 자식 FK는 tenant_id를 포함한다.
- 변경, 감사, outbox는 같은 트랜잭션에 쓴다.

그런데 **key 인증은 tenant를 모르는 상태에서 key_id로 조회해야 한다.** RLS를 우회하지 않으면서 이 조회를 허용할 방법이 필요하다. 드라이버와 migration 도구도 아직 정하지 않았다.

## 결정

### 1. 드라이버와 migration 도구

| 용도 | 선택 | 고정 방식 |
|---|---|---|
| PostgreSQL 드라이버 | `github.com/jackc/pgx/v5` (v5.11.0), pgxpool | `go.mod` |
| migration | `github.com/pressly/goose/v3` (v3.28.0) 라이브러리. SQL 파일 up/down | `go.mod`. 실행은 `cmd/migrate`(migration을 embed한 바이너리, pgx 드라이버만 포함) |

- migration 파일은 `migrations/postgres/NNNNN_name.sql`에 두고 expand → contract 규칙을 따른다 (D06 §07).
- migration은 owner 계정으로 실행한다. 앱은 접근 권한만 받은 role로 접속한다.

### 2. role 구성

| role | 성격 | 권한 |
|---|---|---|
| `montracer_rw` | NOLOGIN 그룹. NOSUPERUSER, NOBYPASSRLS | migration이 테이블별 권한을 이 그룹에 GRANT한다 |
| 앱 login role (로컬은 `montracer_app`) | login 전용 | `montracer_rw`의 멤버. 환경마다 이름이 다를 수 있다 |

- 감사 테이블에는 INSERT와 SELECT만 준다. UPDATE와 DELETE를 막아 append-only로 만든다.

### 2-1. 최소 권한 GRANT

| 테이블 | 앱(`montracer_rw`) 권한 | 이유 |
|---|---|---|
| tenants | SELECT | 생성·region/cell·lifecycle 변경은 provisioning workflow 전용 (D02 §11, D04 §12) |
| memberships | SELECT, INSERT, UPDATE, DELETE | 멤버 관리는 앱 기능 |
| api_keys | SELECT, INSERT, UPDATE(revoked_at, revision) | hash·scope·발급자·tenant는 불변 |
| audit_events, outbox | SELECT, INSERT | append-only |

### 2-2. 기동 시 role 검사

`controldb.Open`은 접속 role이 superuser, BYPASSRLS, 제어 테이블 owner(또는 owner role의 멤버) 중 하나라도 해당하면 `ErrPrivilegedRole`로 기동을 거부한다. 앱 DSN에 관리자 계정을 잘못 넣어 격리가 소리 없이 사라지는 것을 막는다.

### 3. tenant context

- 모든 tenant 범위 작업은 `controldb.WithTenant(ctx, tenant, fn)`를 거친다. 이 함수는 트랜잭션을 열고 `set_config('app.tenant_id', $1, true)`를 parameter binding으로 설정한다(트랜잭션 범위).
- connection pool에서 세션 상태는 공유되지 않는다.
- `app.tenant_id`가 없으면 `current_setting(..., true)`가 NULL이 되어 정책 조건이 거짓이 된다. 결과적으로 아무 행도 보이지 않는다(fail closed).
- DB 실행 예산: 연결마다 `statement_timeout=5s`, `idle_in_transaction_session_timeout=30s`(설계 가정)를 설정한다.
- 오류 분류(ADR 0014): 아래만 의존 서비스 장애(503)로 표시하고, 나머지는 코드 결함으로 보아 500으로 둔다.
  - 연결 오류: 네트워크, 연결 실패, pool 종료, EOF
  - SQLSTATE: 08xxx, 53300, 57P01, 57P03, 25006(failover), 57014(timeout), 40001·40P01(일시 충돌)

### 4. key 인증 조회: 정확히 한 행만 여는 RLS 정책

- `api_keys`에 정책을 하나 더 둔다. 조건은 `key_id = current_setting('app.key_lookup', true)`이고 SELECT에만 적용한다.
- 인증 경로는 트랜잭션 안에서 `app.key_lookup`에 key_id를 설정하고 그 한 행만 읽는다. 그 행의 tenant로 `app.tenant_id`를 설정한 뒤, 발급자의 현재 role을 memberships에서 읽는다 (ADR 0015 §1).
- SECURITY DEFINER 함수나 BYPASSRLS role을 쓰지 않는다. 우회 경로는 "이미 알고 있는 key_id 한 개의 metadata 조회"로만 제한된다.
- key_id는 비밀이 아니다. 하지만 이 경로로 얻는 값(hash, scope)은 인증 검증에만 쓰고 응답으로 내보내지 않는다.
- **잔여 위험:** `app.key_lookup`은 앱 role이 어느 트랜잭션에서든 설정할 수 있는 GUC다. 그래서 "LookupKey에서만 설정한다"는 경계는 DB가 아니라 코드 규율이 지킨다. 이를 보완하려고 단위 테스트가 저장소 전체 Go 코드에서 이 GUC를 설정하는 곳이 `keys.go` 한 곳뿐인지 검사한다.

### 5. 변경 트랜잭션

key 발급과 폐기는 하나의 `WithTenant` 트랜잭션 안에서 아래를 함께 기록한다 (D02 §11, §14).

- `api_keys` 변경
- `audit_events` (category `security`, ADR 0015 §4)
- `outbox` (`key.created`, `key.revoked`)

### 6. 범위 밖 (후속 결정)

- outbox dispatcher의 role과 tenant 간 읽기 정책. dispatcher를 구현할 때 별도 role과 정책으로 추가한다.
- ClickHouse migration.
- **tenant lifecycle에 따른 인증 차단:** suspended·closing·deleted tenant의 key를 거절할지, 상태별로 어떤 동작을 허용할지(D04 §12). 지금 key 인증은 tenant 상태를 보지 않는다. lifecycle을 구현할 때(F09) 결정한다.
- **key 폐기 API의 If-Match/Idempotency-Key:** 저장소의 폐기는 이른 시각 쪽으로만 바뀌어 멱등이다. HTTP 계약은 `DELETE /keys/{id}` API를 구현할 때 적용한다(D02 §20).
- tenant provisioning 전용 role과 workflow.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 드라이버 | pgx v5 | database/sql + lib/pq: 유지보수 모드이고 기능이 부족함 |
| migration | goose (SQL) | golang-migrate: 기능은 비슷하나 Go 함수 migration과 env 치환이 약함. Atlas: 선언형이 강력하지만 RLS·정책 diff의 학습 비용이 큼 |
| 인증 조회 | 한 행 정책 | SECURITY DEFINER 함수: FORCE RLS에서는 owner도 막혀 결국 BYPASSRLS가 필요해짐. 별도 auth DB: 운영 비용이 큼 |

## 결과

- 앱이 인증 전에 볼 수 있는 행은 key_id를 정확히 아는 그 한 행뿐이다.
- goose CLI는 모든 DB 드라이버(70여 개 간접 의존성)를 끌어오므로 쓰지 않는다. `cmd/migrate`는 goose 라이브러리와 pgx만 쓰며, 배포 시 k8s Job으로 같은 바이너리를 실행한다.
- 로컬 init script는 `montracer_rw` 그룹을 만들고 앱 role을 그 멤버로 만든다. 기존 볼륨에는 `make migrate`가 멤버십 GRANT를 멱등적으로 적용한다.

## Rollback

- migration은 down 스크립트로 되돌린다.
- 한 행 정책을 제거하면 key 인증이 동작하지 않는다. 이 경우 대안(별도 auth 저장소)을 새 ADR로 정해야 한다.

## 재검토 조건

- 인증 처리량 때문에 key 조회 cache가 필요해질 때. revoke 전파 60초 제약(D04 §02)과 함께 검토한다.
- outbox dispatcher를 구현할 때.

## 증거

- `internal/controldb` 통합 테스트 (`make test-integration`, CI `go integration (postgres)`):
  - tenant 간 격리, 다른 tenant 감사·outbox 비노출
  - context 없거나 잘못된 tenant context는 0행 또는 오류
  - 한 행 정책의 범위(다른 테이블 비노출, UPDATE 불가)
  - 앱 role이 RLS를 우회할 수 없음, 관리자 DSN으로는 `Open`이 거부됨
  - tenant 생성·상태 변경 불가, key의 불변 컬럼 수정 불가, 감사·outbox 수정·삭제·truncate 거부
  - 발급·폐기 트랜잭션의 원자성, 감사·outbox에 secret 없음
  - 발급자 강등·제거 반영, pool 장애는 503
