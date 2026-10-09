# ADR 0051: alert-worker 실행 경로 — 일정·lease·상태 저장·전이 outbox

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-09 "다음 단계를 진행해줘", E05 단계 B2)
- Owner: API lead
- 날짜: 2026-10-09 (제안·결정)
- 관련: F06, E05 · D02 §11·§17·§21, D04 §01·§02, D06 §05 · 계약 1·4·6 · ADR 0016, 0018, 0049, 0050

## 배경

ADR 0049는 monitor 정의를 저장했고, ADR 0050은 그 정의를 평가하는 순수 계산(`internal/alerting`)을 정했다. 이 ADR은 그 계산을 **주기적으로, 한 번씩, tenant 경계 안에서** 실행하는 경로(`cmd/alert-worker`)를 정한다.

D02 §17이 정한 것은 다음과 같다.
- alert-worker가 평가하고 상태를 바꾸며, 전이는 notification outbox로 나간다.
- 평가는 lease + idempotency로 한 번만 반영한다.
- 알림 중복 억제 key는 tenant + monitor + group + episode_id다.

D02 §11의 상태 테이블은 `alert_instances(group_hash, state, version)`다. 명세가 정하지 않은 것이 있다.
- worker가 tenant를 가로질러 할 일을 어떻게 찾는가(제어 DB는 tenant RLS, ADR 0016)
- worker는 어떤 권한으로 ClickHouse를 읽는가
- slot을 두 번 평가하지 않게 하는 방법
- 정의가 바뀌거나 꺼질 때 열린 경보는 어떻게 되는가
- 평가 기록을 얼마나 남기는가

## 결정

### 1. 일정과 할 일 찾기 (migration 00008, `controldb.AlertStore`)

- `monitor_schedule(tenant, monitor)`에 평가 주기·active·revision·마지막 slot·lease만 둔다. spec 본문은 없다.
  - control-api가 정의 생성·수정·삭제와 **같은 트랜잭션**에서 쓴다. 이미 있는 정의는 migration이 채운다.
  - backfill은 owner 계정에서 FORCE RLS 표를 읽으므로, 그 문장 동안만 `monitors`의 FORCE를 푼다. 옮긴 수가 원천과 다르면 migration을 실패시킨다([PS-0010](../troubleshooting/PS-0010-backfill-empty-under-force-rls.md), 리뷰 반영).
- slot = floor(epoch / evaluation_seconds) × evaluation_seconds. 마지막 slot이 지금 slot보다 이르고, lease가 없거나 만료된 monitor가 할 일이다.
- **tenant를 가로지르는 유일한 조회:** `app.monitor_scan = 'on'`일 때 `monitor_schedule`에 `SELECT`만 여는 RLS 정책이다(읽기 전용 트랜잭션).
  - 보이는 칼럼은 tenant id, monitor id, 평가 주기, active, revision, 마지막 slot, lease token·만료 시각이다. spec·이름·상태·값은 없다. 다른 표(정의·상태·평가 기록·outbox)는 이 설정으로 보이지 않는다. 수정은 tenant 정책만 허용한다.
  - API key 조회(`key_lookup`, ADR 0016)와 같은 방식이다. 다만 `key_lookup`은 key 하나로 좁혀지고 이 정책은 전체 일정을 연다. `montracer_rw`의 어느 세션이든 설정할 수 있다(control-api·query-api 포함). 노출이 일정 칼럼(id·주기·active·revision·slot·lease)뿐이라 받아들이고, alert-worker가 별도 DB 계정을 갖게 되면 정책을 그 role로 좁힌다(재검토 조건).
- 그 밖의 모든 읽기·쓰기(lease·정의·상태·기록·outbox)는 대상 tenant의 `WithTenant` 트랜잭션이다.

### 2. lease와 한 번 평가

- **claim:** `UPDATE … SET lease_token, lease_until WHERE 할 일 조건 RETURNING revision, slot`. 조건부 UPDATE라 동시에 두 worker가 잡을 수 없다.
  - **lease token:** claim마다 새 UUID다. 완료는 이 token으로만 된다(fencing). worker 이름이 겹쳐도(같은 env 고정값) 다른 claim과 섞이지 않는다. worker 이름은 평가 기록에만 남긴다(리뷰 반영).
  - **시간 상한:** claim·저장 각 5초, 조회(watermark·metric) 15초. 기본 lease 1분은 조회 + 저장 2회보다 길어야 한다(설정이 어기면 기동 거부). 완료 저장은 조회와 다른 context다 — 조회가 상한을 다 써도 EVALUATION_ERROR 결과를 쓴다. 쓰지 못하면 이전 상태(예: OK)가 그대로 남는다(계약 6, 리뷰 반영).
  - **시각:** slot과 lease 시각은 worker 시계다. worker 간 시계 차이는 lease보다 훨씬 작다고 가정한다(NTP). 차이가 lease에 가까우면 lease를 일찍 가져가거나 미래 slot을 기록할 수 있다. DB 시각으로 바꾸지 않은 이유: window 끝(ADR 0050)도 같은 시각에서 계산하므로 한 평가 안에서 시계를 섞지 않는다.
- **complete:** 한 트랜잭션에서 다음을 쓴다.
  1. lease 해제와 last_slot 갱신. `lease_token = 이 claim AND revision = 잡을 때 revision AND last_slot < 이 slot`이 아니면 `ErrLeaseLost`이고 아무것도 쓰지 않는다.
  2. group 상태.
  3. 전이 outbox.
  4. 평가 기록.
  5. 오래된 기록 정리.
- **crash:** lease가 만료되면 다른 worker가 같은 slot을 가져간다. 늦게 끝난 원래 worker의 결과는 1.의 조건으로 버려진다.
- **idempotency key:** `monitor_evaluations` PK(tenant, monitor, slot)다. 같은 slot의 기록이 둘이 될 수 없다.
- 밀린 slot은 따라잡지 않는다. 지금 slot 하나만 평가한다(window 끝은 watermark 기준이라 건너뛴 slot의 데이터도 다음 window에 들어간다, ADR 0050 §1).

### 3. ClickHouse 조회 권한 — system principal

- worker는 tenant마다 `authz.NewSystemPrincipal(tenant, "alert-worker", telemetry.read)`로 조회한다.
  - tenant는 `monitor_schedule` 행(즉 정의를 만든 사용자의 인증 principal에서 저장된 값)에서 온다(계약 1).
  - query 계정·row policy(`SQL_montracer_tenant`)·mandatory predicate 경로는 query-api와 같다(ADR 0018, 계약 4).
- system principal은 읽기 전용 action만 받는다. 쓰기·key·감사·삭제·ingest action은 생성자가 거절한다. RBAC 역할·환경 제한이 없는 내부 주체다.
- **environment 제한 key는 monitor를 만들거나 바꿀 수 없다(403, `authz.AuthorizeTenantWide`, 리뷰 반영).** 평가는 tenant 전체 telemetry를 읽는다. 제한 key(예: staging만)가 정의를 만들면 그 key가 볼 수 없는 environment의 값·labels가 경보 상태와 알림으로 돌아온다(D04 §01 "key는 발급 범위보다 강해질 수 없다"). validate·생성·수정·삭제 모두이고, 정의 조회는 telemetry가 아니라 허용한다. 사람(user)은 MVP에서 조직 단위다(environment 제한은 F08). 제한 scope를 정의에 저장하고 그 범위로 평가하는 방식은 F08과 함께 다시 본다.

### 4. 상태 저장과 사건

- `alert_instances(tenant, monitor, group_hash)`: group_hash = group key의 SHA-256 hex(D02 §11). 원문 key와 labels는 `labels` jsonb에 둔다. 상태 머신 필드(ADR 0050 §4)·`last_value`·`last_reason`·`version`을 저장한다.
- **episode_id:** 사건이 열릴 때 새 UUID를 만들고 닫힐 때까지 유지한다. 닫히면 저장 상태에서 지우고, 닫힘 event에는 닫히기 전 id를 싣는다(D02 §17 중복 억제 key).
- **정의 변경(revision)·끄기·삭제:** control-api가 같은 트랜잭션에서 다음을 한다.
  1. 일정의 revision·active를 바꾸고 lease를 지운다. 평가 중이던 결과는 2.의 조건으로 버려진다.
  2. 열린 사건마다 닫힘 event(`reason = monitor_revised | monitor_disabled | monitor_deleted`)를 쓴다.
  3. 그 monitor의 상태를 모두 지운다.

  새 revision은 다음 slot부터 빈 상태로 평가한다(같은 slot이 아직 지금 slot이면 last_slot이 이미 그 slot이라 다음 slot부터다). 조건이 바뀐 정의의 상태를 이어 쓰면 옛 기준의 PENDING·ALERT가 새 기준에 섞인다. 사건을 조용히 지우면 열린 알림이 닫히지 않는다.

### 5. 전이 outbox

- 상태가 바뀌었거나 사건이 열리고 닫힌 group마다 `outbox`에 `alert.state_changed`(schema_version 1)를 같은 트랜잭션에 쓴다. 같은 window를 반복 평가한 것(ADR 0050)과 변화 없는 평가는 쓰지 않는다.
  - payload: group_hash, labels, from, to, opened, closed, episode_id, episode_reason, value, reason, window_end, slot
  - actor: `alert-worker`(인스턴스 이름이 아니다)
- 감사(audit)는 쓰지 않는다. 사람의 변경이 아니고 양이 많다. 정의 변경의 감사는 ADR 0049의 operations 감사가 남긴다.
- 소비자(notification policy·webhook)는 단계 C다. outbox dispatcher는 아직 없다(다른 outbox event와 같다). 그 전까지 event는 outbox에 쌓인다.

### 6. 평가 기록과 보존

- `monitor_evaluations`: slot마다 status(evaluated / no_data / error), reason, window, group 수, ALERT 수, worker, 소요 시간이다.
  - 앱 role에는 추가·조회·삭제만 있고 수정은 없다.
- **보존 7일:** 그 monitor를 평가할 때 7일보다 오래된 기록을 지운다. 1분 주기면 monitor당 약 1만 행이다. 상태 이력은 outbox event가 남긴다.

### 7. 운영 지표와 실행

- 지표:
  - `montracer_alert_evaluations_total{status}`
  - `montracer_alert_state_transitions_total`
  - `montracer_alert_evaluation_duration_seconds`
  - `montracer_alert_lease_lost_total`
  - `montracer_alert_worker_errors_total{stage=scan|evaluate}`(할 일 찾기 실패, claim·저장 실패)
  - `montracer_alert_last_scan_timestamp_seconds`(할 일 찾기 성공 heartbeat)
- 경보(RB01에 절이 있다)
  - `MontracerAlertWorkerFailing`(10분째 실패, page — 평가가 멈추면 고객 경보가 침묵한다)
  - `MontracerAlertWorkerNotRunning`(heartbeat 약 4분 정지 또는 지표 없음, page — 오류 없이 멈춘 경우·배포 누락, 리뷰 반영)
  - `MontracerAlertEvaluationErrorsHigh`(EVALUATION_ERROR > 10% 15분, ticket)
- tick 5초, batch 100, 동시 평가 4다. monitor 하나의 실패는 다른 monitor를 막지 않는다. 그 slot은 lease 만료 뒤 다시 시도된다.
- 종료(SIGTERM) 중에는 새 평가를 시작하지 않고, 종료로 끊긴 조회의 결과는 쓰지 않는다. 종료는 조회 실패가 아니다(거짓 EVALUATION_ERROR·전이 event를 만들지 않음). 그 slot은 lease 만료 뒤 다른 worker가 평가한다(재검토 반영).
- 조회 실패는 worker 오류가 아니라 평가 결과(`error`·`query_failed`, EVALUATION_ERROR)로 기록한다(D02 §21). 저장된 정의를 해석하지 못하면 `invalid_spec` 오류 결과이고, 저장된 group은 모두 EVALUATION_ERROR가 된다(값 없음, 사건·위반 시작 유지, 복구 연속 끊음 — 조회 실패와 같은 규칙, 리뷰 반영).
- `make dev`가 alert-worker를 함께 띄운다(지표 19468).

## 외부 사례 근거 (2026-10-09 확인)

| 사례 | 내용 | 반영 |
|---|---|---|
| Grafana Mimir ruler ([docs](https://grafana.com/docs/mimir/latest/references/architecture/components/ruler/)) | ruler가 tenant별 rule group을 여러 replica에 나눠 평가하고, rule group 하나는 한 replica만 평가한다 | 결정 2: monitor·slot 하나를 worker 하나만 평가(hash ring 대신 DB lease — 제어 DB가 이미 단일 원천) |
| Grafana Alerting HA ([docs](https://grafana.com/docs/grafana/latest/alerting/set-up/configure-high-availability/)) | 모든 instance가 같은 rule을 평가하고 알림 단계에서 중복을 없앤다 | 기각: 평가 비용이 instance 수만큼 늘고 상태 저장이 경쟁한다. 대신 알림 중복 억제 key(episode_id)는 둔다 |
| Transactional outbox ([microservices.io](https://microservices.io/patterns/data/transactional-outbox.html)) | 상태 변경과 event를 같은 DB 트랜잭션에 쓰고 따로 전달한다 | 결정 5: 상태와 전이 event 한 트랜잭션(ADR 0016 outbox 재사용) |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 정의 변경 반영 | control-api가 같은 트랜잭션에서 schedule·상태를 바꿈 | `monitor.*` outbox 소비(ADR 0049가 예상한 방식): dispatcher가 없고, 소비가 늦는 동안 옛 revision으로 평가한다 |
| 할 일 찾기 | schedule 표 + 좁은 scan 정책 | tenant 목록을 돌며 tenant마다 조회: tenant 수만큼 왕복. 별도 superuser 계정: 모든 표가 열린다 |
| 한 번 평가 | 조건부 UPDATE lease + slot PK | `FOR UPDATE SKIP LOCKED`만: 평가(ClickHouse 조회) 동안 트랜잭션을 열어 둬야 한다. Kafka partition 소유: 일정이 제어 DB에 있어 두 원천이 된다 |
| ClickHouse 권한 | tenant마다 system principal(읽기만) | 서비스 계정 tenant 무관 조회: row policy·mandatory predicate 경로를 벗어난다(계약 1·4) |
| revision 변경 | 상태 초기화 + 열린 사건 닫힘 event | 상태 이어 쓰기: 옛 조건의 PENDING·ALERT가 섞인다. 조용히 삭제: 열린 알림이 닫히지 않는다 |
| 밀린 slot | 지금 slot만 | 모두 따라잡기: 장애 뒤 같은 데이터를 여러 번 평가하고 전이가 몰린다 |
| 완료 fencing | claim마다 lease token | worker 이름: 이름이 겹치면 만료 뒤 다른 claim의 lease로 완료된다 |
| 제한 key의 monitor | 생성·수정 거절 | 생성자 scope로 평가: 정의마다 scope를 저장·검증해야 하고 사람 scope(F08)가 아직 없다 |
| 전이 감사 | outbox만 | 감사도: 사람의 행위가 아닌 대량 기록이 감사를 덮는다 |

## 결과

- monitor가 실제로 평가되고 상태·사건·전이 event가 남는다. 알림 전송(단계 C)·화면(S09, 단계 D)·dry-run(B3)은 아직 없다.
- **schema:** PostgreSQL migration 00008(`monitor_schedule`, `monitor_evaluations`, `alert_instances`, RLS ENABLE+FORCE, 최소 GRANT, 기존 정의 backfill)
- **control-api:** 정의 생성·수정·삭제가 일정·상태를 같은 트랜잭션에서 바꾼다. API 응답 변화는 없다.
- **authz:** `KindSystem` principal(읽기 전용 action만), `AuthorizeTenantWide`(environment 제한 key 거절)
- **API 변화:** environment 제한 API key의 monitor validate·생성·수정·삭제가 403이다(이전에는 허용). 제한 없는 key와 사람은 같다.
- tenant 영향: 할 일 찾기만 tenant를 가로지르고 일정 칼럼(id·주기·revision·slot·lease)만 보인다. 나머지는 tenant RLS다.
- retention 영향: 평가 기록 7일. 상태는 정의가 있는 동안 유지된다.

## Rollout

1. migration 00008(일정 표, 기존 정의 backfill)
2. control-api(정의 변경이 일정·상태를 함께 바꿈). 1과 2 사이에 구 control-api가 만든 monitor는 일정이 없다 → 2 뒤에 RB01 "평가 일정이 빠진 monitor 찾기"를 한 번 돌린다.
3. 운영 경보 규칙과 alert-worker(여러 인스턴스 가능, `MONTRACER_ALERT_WORKER_ID`는 인스턴스마다). 규칙을 먼저 적용하면 `MontracerAlertWorkerNotRunning`이 울린다.

## Rollback

- alert-worker 배포를 멈추면 평가가 멈춘다. 일정·상태는 남고 정의 API는 그대로 동작한다. 다시 띄우면 지금 slot부터 평가한다.
- migration 00008 down은 세 표를 지운다(상태·기록 유실, 정의는 남음).

## 재검토 조건

- monitor 수가 늘어 scan(`ORDER BY last_slot`, partial index)이나 worker 처리량이 부족할 때: tenant·monitor hash로 worker를 나누는 sharding
- 오래 사라진 group의 NO_DATA 상태가 쌓일 때: 정리 규칙(ADR 0050 §5와 함께)
- 단계 C: outbox event 소비(notification policy·webhook·HMAC·SSRF), 중복 억제
- 단계 B3: 같은 계산으로 24시간 dry-run
- alert-worker가 별도 DB 계정을 가질 때: scan 정책을 그 role로 좁힌다(`TO …`)
- F08(사람의 environment scope): 제한 principal의 monitor를 거절하는 대신 정의에 scope를 저장하고 그 범위로 평가
- worker 시계 차이가 문제가 되면: lease·slot 기준을 DB 시각으로

## 증거

- `internal/alertworker/worker_test.go`(fake 저장소)
  - 사건 수명: 열림 새 id → 유지 → RECOVERING → 닫힘 event에 같은 id·저장 상태 id 없음
  - system principal(tenant·읽기·subject)
  - 조회 실패 → EVALUATION_ERROR 기록(값 없음), 조회가 상한까지 멈춰도 오류 결과 저장(완료는 다른 context)
  - watermark 멈춤 → no_data
  - 다른 worker가 잡음 → 조회 없음
  - lease 잃음 → 쓰지 않음·지표
  - 저장 정의 해석 실패 → invalid_spec, 기존 ALERT group이 EVALUATION_ERROR로(사건·위반 시작 유지, 값 없음), 반복은 변화 없음
  - 종료로 끊긴 조회 → 결과를 쓰지 않음·오류로 세지 않음
  - 저장 실패 → `evaluate` 단계 오류로 셈, heartbeat 기록, lease가 시간 상한보다 짧으면 기동 거부
- `internal/controldb/alerting_integration_test.go`(PostgreSQL)
  - lease 독점, 다른 token 완료 거절, 끝난 slot 재claim 없음, 상태 왕복(원문 key·labels·사건 id)
  - lease 만료 뒤 인계와 늦은 결과 폐기, 변화 없는 평가 event 없음
  - revision 변경 → 평가 중 결과 폐기·닫힘 event(`monitor_revised`)·상태 초기화·새 revision 빈 상태
  - 끄기 → 할 일 아님·닫힘 event(`monitor_disabled`), 다시 켜면 새 revision 빈 상태
  - 삭제 → 할 일 아님·닫힘 event(`monitor_deleted`)
  - 7일 보존 정리
  - 격리: 다른 tenant claim·완료 불가, 세 표 안 보임, tenant context 없이 안 보임
  - scan 설정은 schedule SELECT만(정의·상태·기록·outbox 안 보임, UPDATE 0행)
  - 앱 role은 평가 기록 UPDATE·schedule tenant_id UPDATE 불가(42501)
- `internal/alertworker/integration_test.go`(PostgreSQL + ClickHouse `metric_1m`, query 계정): 5% 오류 tenant는 ALERT·열림 event(값 0.05, labels), 정확히 2% tenant는 OK·event 없음(D06 §05 경계), 같은 slot 재평가 없음
- `internal/authz/principal_system_test.go`: system principal은 telemetry.read만, 쓰기·key·감사·삭제·ingest 거절, 잘못된 tenant·subject 거절
- PostgreSQL migration up → down → up. CI는 superuser가 아닌 owner로도 돌리고, 기존 monitor 2개의 backfill을 확인한다(PS-0010)
- `internal/controlapi/monitors_test.go` `TestMonitorWriteRejectsEnvironmentRestrictedKey`: 제한 key의 validate·생성·수정·삭제 403, 제한 없는 key 생성 201, 제한 key 조회 200
- `deploy/prometheus/rules/montracer.rules.test.yml`: 저장 실패 지속 → page, 한 번 실패 뒤 회복 → 없음, heartbeat 정지·지표 없음 → page, heartbeat 계속 → 없음, 오류 20% → ticket, 오류 2%·결측 → 없음
- spec-reviewer: P1 3건(owner 계정 backfill 0행 — PS-0010, environment 제한 key의 tenant 전체 평가, 오류 없는 정지 경보 없음)·P2 9건 반영, 재검토 P2 3건(종료를 조회 실패로 기록, heartbeat 경보의 ClickHouse 지연 원인 안내, scan 노출 문구) 반영. F06 registry 상태는 Gate 증거 전이라 `planned` 유지
