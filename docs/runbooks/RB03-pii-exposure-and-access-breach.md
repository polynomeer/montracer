# RB03 개인정보 유출과 권한 침해

- 원문: D04 §11 RB03, PII 정책: D04 §03, 격리·RBAC: D04 §01, 보존·삭제: D04 §04
- 관련 설계: ADR 0015·0016(제어 DB RLS·audit), 0018(ClickHouse row policy), 0019(redaction), 0031(synthetic probe)
- 경보 규칙: [`deploy/prometheus/rules/montracer.rules.yml`](../../deploy/prometheus/rules/montracer.rules.yml) (`security: "true"` label)

## 먼저 알아둘 원칙

1. **SEV 판단(D04 §11 공통 체계)**
   - cross-tenant 노출은 **SEV1**이다. 감지 후 5분 안에 incident commander, technical lead, **보안 담당자**를 지정한다. 15분마다 영향·완화·다음 시각을 갱신한다.
   - redaction 실패로 PII가 저장된 경우도 노출 범위가 확정될 때까지 SEV1로 다룬다.
2. **민감 payload를 옮기지 않는다.** 티켓·채팅·incident 문서에는 tenant UUID, key ID, 시간 범위, 건수, query_id 같은 비민감 metadata만 적는다. span·log 본문, 속성 값, 응답 본문은 복사하지 않는다.
3. **증거는 보전하되 원문은 만들지 않는다.** 접근 audit(`audit_events`)와 비민감 metadata는 제한 저장소에 보전한다. "분석용"으로 원문을 따로 떠 두지 않는다(D04 §03, 계약 3).
4. **단순 key 교체만으로 사건을 종료하지 않는다(D04 §11).** 종료 조건은 아래 "복구 확인" 세 가지다.
5. **운영자의 tenant 데이터 접근은 break-glass로 기록한다(D04 §01).**
   - 대상: 이 runbook의 모든 관리자 계정 조회와 삭제(`system.query_log`, fixture 검색, DELETE).
   - key 목록·폐기는 `montracer-admin`이 grant(사유·승인자·ticket·30분)를 강제하고 tenant 감사에 남긴다(ADR 0033).
   - 실행 전에 incident 기록에 사유, 승인자(보안 담당자), tenant scope, 시작·종료 시각(최대 30분)을 적는다.
   - **현재 공백:** key 밖의 접근(ClickHouse 조회·삭제)을 강제·감사하는 break-glass 도구는 아직 없다. 그 접근은 기록이 수동이다.
6. **`system.query_log`의 `query` 컬럼은 masking이 확인된 행만 본다.** ClickHouse `query_masking_rules`가 문자열 값(trace_id, metric 이름, filter 값 등)을 `'?'`로 바꾼다(ADR 0032).
   - 먼저 masking이 켜져 있는지 확인한다. 관리자 계정으로 `SELECT 'rb03-masking-check'`를 실행한 뒤, `SYSTEM FLUSH LOGS` 후 그 query의 `query` 컬럼이 `SELECT '?'`인지 본다. 값이 보이면 `query` 컬럼은 쓰지 않는다.
   - masking 설정 배포 이전에 기록된 행에는 값이 그대로 있다. 그 구간의 `query` 컬럼은 조회·복사·export하지 않는다.
   - `exception` 문구에는 값이 따옴표 없이 남을 수 있다(예: `Cannot parse uuid <값>`, ADR 0032). `exception_code`만 쓴다.

## 탐지 신호

| 신호 | 의미 | 절 |
|---|---|---|
| `MontracerSyntheticRedactionFailing` | probe의 이메일 표본이 가려지지 않은 채 조회됨 | 아래 |
| `MontracerSyntheticIsolationFailing` | 다른 probe tenant key로 probe trace가 200으로 조회됨 | 아래 |
| `MontracerSyntheticIsolationUnmonitored` | 격리 검사가 30분째 성공하지 못함(감시 공백) | 아래 |
| 고객·내부 신고 | 다른 조직 데이터가 보임, 저장되면 안 될 값이 보임 | "공통 대응" |
| 보안 fixture 회귀 실패(D04 §03) | release 전후 fixture가 저장소에서 검색됨 | "공통 대응" |

---

### MontracerSyntheticRedactionFailing

- **탐지:** probe가 root span 속성에 넣은 이메일 표본(`example.com`, 실제 사람 아님)이 2회 연속 원문으로 조회됐거나 가림 표식 `[REDACTED:email]`이 없다. 앞 검사가 실패해 평가하지 못한 경우는 이 경보를 울리지 않는다(ADR 0031 `blocked`).
- **의미:** ingress redaction(ADR 0019)이 적용되지 않은 채 저장되고 있다. 같은 시각 고객 데이터도 같은 상태일 수 있다.
- **즉시 조치**
  1. **최근 ingress release를 확인하고, 시작 시각과 겹치면 ingress를 직전 digest로 되돌린다.** redaction 정책은 코드(`internal/telemetry/redact`, policy version)로 배포되므로 정책 되돌리기 = ingress 되돌리기다.
  2. 되돌린 뒤 probe redaction check가 다시 성공하는지 본다(`montracer_probe_consecutive_successes{check="redaction"}`).
  3. **영향 범위를 시간으로 확정한다.** 시작은 마지막 redaction 성공 시각(`montracer_probe_last_success_timestamp_seconds{check="redaction"}`)이고, 끝은 되돌린 뒤 첫 성공 시각이다. 그 구간에 수집된 모든 tenant가 후보다.
  4. 아래 "영향 저장소와 정리"를 따른다.
- **주의:** probe 표본은 가짜 값이다. 원문이 조회됐다는 사실 자체가 증거이므로 응답을 저장하거나 캡처하지 않는다.

### MontracerSyntheticIsolationFailing

- **탐지:** 다른 probe tenant의 API key로 probe trace를 조회했더니 **200**으로 보였다(2회 연속). **SEV1 후보**다.
  - 401·403(다른 key 만료·폐기), 429, 5xx, 네트워크 오류는 격리를 판정하지 못한 결과다. 이 경보가 아니라 `MontracerSyntheticIsolationUnmonitored`로 간다.
- **의미:** query 경로의 tenant 격리가 깨졌다. 가능한 원인은 세 계층이다.
  - **인증:** key → tenant principal 도출(`internal/authz`, `internal/controldb` key 조회)이 잘못된 tenant를 준다. 이 경우 아래 두 계층은 그 tenant를 그대로 따르므로 막지 못한다.
  - **query compiler:** tenant mandatory predicate가 빠졌다.
  - **ClickHouse row policy(ADR 0018):** 정책이 빠졌거나 잘못된 role에 적용됐다.
- **즉시 조치**
  1. probe 로그 `synthetic probe check failed`에서 `check=isolation`의 `reason`이 `other tenant got status 200`인지 확인한다. 그렇다면 보안 담당자를 바로 부른다.
  2. 최근 query-api·인증 코드 release와 ClickHouse·제어 DB migration을 확인하고, **query-api를 직전 digest로 되돌린다.**
  3. row policy가 그대로 있는지 확인한다(관리자 계정, 읽기만).
     ```sql
     SELECT short_name, database, table, select_filter, apply_to_list
     FROM system.row_policies WHERE short_name = 'tenant_isolation';
     ```
     `spans_local`·`logs_local`·`trace_lookup`·`metric_points`·`metric_1m`·`metric_1h`마다 정책이 있고 `montracer_ch_query`에 적용돼 있어야 한다. 전체 행을 여는 `rollup_all_tenants`는 `montracer_ch_rollup`에만 적용돼야 한다(`short_name = 'rollup_all_tenants'`로 같이 확인). 빠졌으면 migration 이력에서 원인을 찾는다. 수동으로 다시 만들기 전에 보안 담당자와 범위를 확정한다(증거 보전).
  4. 원인이 확정될 때까지 query-api를 내리는 것도 선택지다. 조회 중단은 SEV2 수준 영향이지만, 노출 지속보다 낫다. incident commander가 결정한다.
  5. 노출 범위를 찾는다. 해당 구간 query 계정의 query 기록을 tenant 설정별로 본다.
     ```sql
     SELECT event_time, query_id, Settings['SQL_montracer_tenant'] AS tenant, read_rows, result_rows
     FROM system.query_log
     WHERE user = '<query_user>' AND type = 'QueryFinish'
       AND event_time BETWEEN '<시작>' AND '<끝>'
     ORDER BY event_time;
     ```
     요청한 tenant(설정)와 반환된 행의 tenant가 다른 경우가 노출이다. 반환 행의 tenant는 query_log에 없으므로, 재현 시험으로 판단한다. `query` 컬럼은 원칙 6의 확인을 거친 행만 쓴다.
- **복구 확인:** probe isolation check 연속 3회 성공, 그리고 격리 negative test 통과. `internal/controldb`·`internal/telemetrystore` 통합 테스트의 tenant 분리 시험과 `make test-isolation`(후속)이 해당한다.

### MontracerSyntheticIsolationUnmonitored

- **탐지:** probe의 isolation 검사가 30분 동안 한 번도 성공하지 못했다. 격리 위반이 아니라 **격리 감시가 꺼진 상태**다.
- **원인과 조치**
  - `outcome="skipped"`만 증가: `MONTRACER_PROBE_OTHER_API_KEY`가 구성되지 않았다. production에서는 반드시 구성한다(ADR 0031).
  - `outcome="blocked"`이고 reason이 `status 401·403`: 다른 probe tenant의 key가 만료·폐기됐다. 새 key를 secret manager에 넣는다.
  - reason이 `status 5xx`·`query unreachable`이거나 trace 검사도 실패: 조회 경로 장애다. RB01·RB02를 따른다.
- **복구 확인:** `montracer_probe_checks_total{check="isolation",outcome="ok"}`가 다시 증가한다.

---

## 공통 대응 (신고·fixture 회귀 포함)

1. **범위 확인:** 의심 tenant, key ID, policy version, 시간 범위를 적는다.
   - key ID는 break-glass 목록 조회로 본다(hash·원문 없음, 조회는 tenant 감사에 남는다, ADR 0033).
     ```bash
     montracer-admin keys list --tenant <tenant UUID> --approver <보안 담당자 ID> --ticket <INC-…> --reason "<사유, 고객 데이터 없이>"
     ```
   - policy version은 지금 Kafka record header `mt-policy-version`에만 있다. raw topic의 payload에는 가려지지 않은 PII가 있을 수 있다. 그래서 **header만 출력하는 형식으로만 읽는다**. 예: `kcat -C -b $BROKERS -t <topic> -p <partition> -o <offset> -c 1 -f '%h\n'`. payload 출력(`%s`, 기본 형식)과 `kafka-console-consumer`의 value 출력은 금지다.
   - **후속:** policy version을 ingress 기동 로그나 지표로 노출하면 이 단계가 필요 없어진다.
2. **차단**
   - **key:** 의심 key를 break-glass로 즉시 폐기한다(ADR 0033).
     ```bash
     montracer-admin keys revoke --tenant <tenant UUID> --key-id <key ID> --approver <보안 담당자 ID> --ticket <INC-…> --reason "<사유>" --yes
     ```
     - bastion/session manager에서 실행한다. 운영자 ID는 session의 `MONTRACER_OPERATOR_ID`에서 오고, 승인자는 운영자와 달라야 한다.
     - 폐기는 tenant 감사에 `key.revoked`(actor operator, 사유·승인자·ticket 포함)로 남고, 같은 outbox event가 나간다. 인증은 제어 DB를 직접 조회하므로 즉시 거절된다.
     - `already revoked`면 이미 차단된 key다. `not found`면 key ID나 tenant를 다시 확인한다. 이 시도도 감사에 남는다.
     - 사유는 고객이 감사에서 읽는다는 전제로 쓴다. 고객 데이터·payload를 적지 않는다.
     - **DB에서 `revoked_at`을 직접 고치지 않는다.** audit와 outbox가 남지 않아 폐기 사실을 증명할 수 없다.
     - tenant 관리자가 직접 폐기할 control-api는 아직 없다.
   - **정책:** 잘못된 policy를 이전 검증본으로 되돌린다(ingress digest 되돌리기, 위 redaction 절).
   - **stream·cache·export 무효화:** live tail·query cache·export는 아직 없다. 생기면 이 단계에 추가한다.
   - **노출 데이터의 조회 차단:** D04 §04는 삭제 요청 뒤 15분 안에 query 접근 차단(tombstone)을 요구한다. 그런데 tombstone·삭제 job이 아직 없다. **현재 공백:** 아래 정리(DELETE)를 마칠 때까지 노출 데이터가 해당 tenant의 조회에 계속 보인다. 이 공백을 incident 기록에 적고, 정리를 우선한다.
3. **증거 보전:** 다음을 제한 저장소로 export한다. 모두 metadata이고 payload는 없다.
   - `audit_events` 해당 구간(operations·security)
   - ingress 로그 `otlp request`(tenant·건수·사유)
   - ClickHouse `system.query_log`의 metadata 컬럼(query_id·tenant 설정·행 수·`normalized_query_hash`). `query` 컬럼은 원칙 6의 확인을 거친 행만 포함한다
4. **보안 담당자가 고객 통지 대상과 시점을 정한다.** 운영자가 직접 고객에게 노출 사실을 알리지 않는다.

## 영향 저장소와 정리

redaction은 Kafka에 쓰기 **전에** 실행된다(계약 3). 그래서 redaction이 빠진 구간의 데이터는 모든 하류 저장소에 있다.

| 저장소 | 남는 기간 | 정리 |
|---|---|---|
| Kafka raw topic | 24시간 보존 | record 단위 삭제 수단이 없다. 보존 만료를 기다리고 만료 시각을 기록한다. 그 전에 replay하지 않는다 |
| ClickHouse 원본(`spans_local`·`logs_local`·`metric_points`·`trace_lookup`) | tenant 보존 기간 | 아래 |
| ClickHouse 집계(`metric_1m`·`metric_1h`) | 90일·395일 | 집계에는 attribute 값이 label로 들어간다. 원본과 같은 범위를 정리한다 |
| quarantine(`ingest_quarantine`) | — | payload를 저장하지 않으므로 대상이 아니다(ADR 0021) |
| 제어 DB 등록부(`metric_series`·`metric_label_values`) | 2시간 정리 | 값은 해시만 있다. 대상이 아니다 |

- **삭제 job(D04 §04)과 삭제 proof는 아직 없다.** 그때까지 ClickHouse 정리는 **보안 담당자 승인 뒤** 관리자 계정이 아래 순서로 실행한다.
  1. **먼저 소비 완료를 확인한다.** 정리 구간 끝 시각 이후의 record까지 worker가 commit했는지 본다(RB01 공통 확인의 consumer group). 아직 lag 중인 record는 정리 뒤에 다시 들어온다. 이 24시간 동안 offset을 되돌리지 않는다(RB01 금지 사항).
  2. **범위 기준을 고른다.** redaction 누락 구간은 **수집 시각** 기준이다.
     - `spans_local`은 `received_at`(수집 시각)으로 거른다.
     - `logs_local`·`metric_points`에는 수집 시각 컬럼이 없다. `event_time`(metric은 `end_time`)으로 구간을 넓혀 잡을 수밖에 없다. 그러면 늦게 도착했거나 과거 시각으로 찍힌 event는 빠지고, 무관한 event는 함께 지워질 수 있다. 이 한계와 선택한 여유 폭을 incident에 기록한다.
     - 집계(`metric_1m`·`metric_1h`)는 원본 범위를 덮는 window 전체를 고른다.
  3. **실행 전 건수를 센다.** 예: `SELECT count() FROM spans_local WHERE tenant_id = '<tenant>' AND received_at BETWEEN '<시작>' AND '<끝>'`
  4. **삭제한다.** 예: `DELETE FROM spans_local WHERE tenant_id = '<tenant>' AND received_at BETWEEN '<시작>' AND '<끝>'`
     - production(Replicated·Distributed)에서는 local 테이블에 `ON CLUSTER <cluster>`로 실행한다.
     - `system.mutations`에서 모든 replica의 `is_done = 1`을 확인한다.
  5. **실행 후 0건을 확인한다.** 실행한 SQL, 전후 건수, mutation 완료 기록을 증거로 남긴다.
  - 범위를 넓히거나 TRUNCATE하지 않는다(RB01 금지 사항).
- backup·archive는 아직 없다. 생기면 같은 범위를 추적한다.

## 복구 확인 (D04 §11 RB03)

세 가지를 모두 만족해야 사건을 종료한다.

1. **fixture 검색 불가:** 노출 원인이 된 값의 형태(probe 표본 등)가 모든 저장소에서 검색되지 않는다. 확인 query의 결과는 건수만 기록한다.
2. **교차 조직 negative test 통과:** probe isolation 연속 성공, 격리 통합 테스트 통과.
3. **삭제 proof 완료:** 위 정리 절차의 실행 전후 건수 기록. 삭제 job이 생기면 job의 proof로 대체한다.

## 사후 기록

- break-glass를 썼다면 tenant 감사의 `actor_kind='operator'` 행(approver·ticket)을 ticket의 실제 승인 기록과 대조한다. 감사의 approver는 운영자가 입력한 값이라 검증되지 않았기 때문이다(ADR 0033 §4).
- 2영업일 안에 타임라인, 탐지 공백, 영향 tenant·건수(값 없이), 재발 방지 owner와 기한을 기록한다.
- probe가 먼저 잡았는지, 고객 신고가 먼저였는지 기록한다. 후자면 탐지 공백이다.
