# ADR 0029: metric cardinality quota — 금지 dimension, label 수, 활성 series 상한

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0028과 같은 위임). 근거는 §6
- 날짜: 2026-10-05 (제안·결정)
- 관련: F07, E03 · D02 §04, §10, §22 · D06 리스크(metric cardinality 폭증, 높음·높음) · ADR 0017, 0019, 0020, 0024

## 배경

D02 §10이 정한 것은 다음과 같다.

- 활성 series는 최근 1시간 안에 관측된 stream이다.
- 기본 quota는 조직별 100k, metric별 label key 20개, key당 활성 값 100개다.
- user_id·session_id·request_id·trace_id는 metric dimension으로 쓸 수 없다.
- **새 series가 상한을 넘으면 기존 series는 계속 수용하고 신규 조합만 거절한다.**
- **숨은 자동 attribute 삭제로 서로 다른 series를 합치지 않는다.**

D02 §22는 record별 quota 결과를 partial success로 모으라고 한다.

지금은 한 tenant가 사용자 ID 같은 높은 cardinality 속성으로 series를 무한히 만들 수 있다. 그러면 저장소와 rollup(ADR 0026)의 비용이 함께 커진다.

## 결정

### 1. 판정 위치와 순서 (ingress, envelope 전)

```
검증 → environment 범위 → 예약 속성 제거
→ ① dimension 규칙 (금지 key, label 수)      ← redaction 전, key 이름만 본다
→ redaction
→ rate quota (ADR 0024)
→ ② 활성 series 상한                          ← redaction 뒤 identity = 저장될 identity
→ envelope → Kafka append
```

- 거절은 **point 단위 partial success**다(D02 §22). 같은 요청의 다른 point는 append한다.
  - 사유는 `forbidden_metric_dimension`, `too_many_metric_labels`, `series_limit_exceeded`이고 회계·지표에 남는다.
- **① 금지 dimension은 redaction 전에 본다(구현 중 발견).** redaction 정책(ADR 0019)은 `user_id` 같은 key를 지운다. 지운 뒤에 판정하면 사용자별로 다른 series가 하나로 합쳐진 채 통과한다. 이것이 D02 §10이 금지한 "숨은 자동 attribute 삭제"다.
  - key 이름만 보고, 거절한 point는 저장하지 않으므로 PII 계약(최초 영속 저장 전 제거)과 충돌하지 않는다.
- **속성을 지워서 통과시키지 않는다.** 규칙에 걸리면 point 전체를 거절한다.
- series identity는 envelope와 **같은 함수**로 미리 계산한다(`envelope.MetricStreams`). 판정한 series와 저장되는 series가 같다(시험으로 고정).

### 2. dimension 규칙

| 규칙 | 기준 |
|---|---|
| 금지 key | `user_id`, `session_id`, `request_id`, `trace_id`와 OTel·흔한 표기(`user.id`, `enduser.id`, `session.id`, `request.id`, `http.request.id`, `trace.id`, `span_id`…). 대소문자 무시. point·resource·scope 속성 모두(셋 다 identity에 들어간다) |
| label 수 | point 속성 key가 20개를 넘으면 거절 |
| metric 이름 | 비었거나 255자를 넘으면 `invalid_metric_name`으로 거절(등록부 컬럼 제약과 같다) |

### 3. 활성 series 상한 (기본 100k, tenant별 override)

- **공유 등록부:** 제어 DB `metric_series(tenant_id, stream_id, metric_name, first_seen, last_seen)`. RLS(ENABLE+FORCE)이고 `montracer_rw`의 최소 권한만 둔다(ADR 0016).
  - stream fingerprint(128-bit)와 metric 이름만 저장한다. **속성 값은 저장하지 않는다.**
- **판정:** 최근 1시간 안의 `last_seen`이면 활성이다.
  - 요청의 series 중 **등록부에 활성으로 있으면 기존**이다. 다른 replica가 등록한 series도 포함되며, 한도와 무관하게 받는다.
  - 신규는 `상한 − 활성 수`만큼 요청 순서대로 등록하고, 나머지 series의 point는 모두 거절한다.
- **cache:** 한 번 기록한 series는 10분 동안 등록부를 조회하지 않는다. 10분이 지나면 `last_seen`을 갱신한다. 활성 수는 15초 동안 cache한다.
  - 정상 상태에서는 신규 series와 10분마다 한 번의 갱신만 DB에 간다.
- **soft limit:** 활성 수를 replica마다 15초 cache한다. 그동안 다른 replica의 신규 등록은 보이지 않는다.
  - 최악의 경우 상한을 **(replica 수 − 1) × (15초 동안 한 replica가 받은 신규 series 수)**만큼 넘을 수 있다. 신규 폭증이 동시에 여러 replica에 오는 cold start에서 가장 크다.
  - 기존 series 판정은 등록부가 원천이라 replica마다 다르게 나오지 않는다.
- **잠금과 시간 상한:** 등록부 호출(Known·ActiveCount·Touch)은 tenant 잠금 **밖**에서 하고, 호출마다 2초 상한을 둔다. 잠금은 자리 계산에만 쓴다.
  - 잠금을 잡은 채 DB를 기다리면, DB가 느릴 때 그 tenant 요청이 모두 줄을 서 instance 동시 처리 상한을 채운다(리뷰에서 발견).
  - 등록에 실패하면 세어 둔 자리를 돌려준다.
- **장애:** 등록부(제어 DB) 장애·시간 초과면 판정할 수 없다. 받지 않는다: 503 + Retry-After(fail closed, 인증 저장소 장애와 같은 방식).
  - **그 밖의 등록부 오류(제약 위반 등)는 500이다.** 재시도해도 같은 결과이므로 503으로 무한 재전송을 부르지 않는다(리뷰에서 발견).
- **rate quota와의 순서:** rate quota(ADR 0024)를 먼저 적용한다. 429가 나는 요청은 등록부를 건드리지 않는다. 대가로 series 상한에 걸려 거절된 point도 rate token을 쓴다(보수적인 쪽).
- **정리:** 10분마다 다룬 tenant의 2시간 넘게 안 보인 행을 지운다. 판정에는 영향이 없다(공간 정리).
- **override:** quota overrides 파일의 `metrics.active_series`(ADR 0024 §4)이고, 기본값은 `MONTRACER_QUOTA_ACTIVE_SERIES`다.

### 4. 아직 하지 않는 것

- **key당 활성 값 100개(D02 §10):** ADR 0030에서 구현했다.
- **preview·명시적 dimension 정책 변경, 조직별 top offending key(D02 §10):** 관리 API·UI(F07)와 함께 만든다.
- **tenant별 거절 집계:** usage 원장(D04 §08)이 맡는다. 지표에는 tenant label이 없다(ADR 0023).

### 5. 회계

- `rejected{reason=forbidden_metric_dimension|too_many_metric_labels}`는 영구 거절(permanent_reject)이다.
- `series_limit_exceeded`는 quota 거절이다(D02 §21: `valid_before_quota = quota_reject + durable_accepted`).
- 단위는 envelope(data point) 수다.

### 6. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §3 tenant 활성 series 상한·override | Grafana Mimir limits ([GEM limits](https://grafana.com/docs/enterprise-metrics/latest/configure/config-gem/limits.md)) | `max_global_series_per_user`(tenant별 series 상한, runtime override). 넘으면 push에 400과 "per-user series limit exceeded" | 채택: tenant 상한·override. 응답은 point 단위 partial success(D02 §22) |
| §2 label 수 상한 | Grafana Mimir `max_label_names_per_series` (위, 기본 30) | series 하나의 label 이름 수 제한 | 채택: 20(D02 §10) |
| §3 공유 등록부 | Mimir의 global limit (ADR 0024 §7과 같은 문서) | 전역 한도를 replica에 나눠 근사 적용 | 다르게: 이 제품의 ingress는 series별 sharding(ring)이 없다. 그래서 replica가 공유하는 등록부로 "기존 series" 판정을 일관되게 한다 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 판정 위치 | ingress(envelope 전) | worker(저장 시): 이미 ACK한 뒤라 client에 알릴 수 없다. partial success(D02 §22)를 줄 수 없다 |
| 공유 상태 | 제어 DB 등록부 + replica cache | replica별 메모리만: replica마다 "기존" 판정이 달라 기존 series가 거절된다. ClickHouse 조회: 요청 경로에 분석 저장소 지연이 붙고 query 계정 권한이 ingress에 필요하다. Redis: 새 의존성 |
| 금지 dimension 처리 | point 거절(redaction 전 판정) | key 삭제 후 수용: 서로 다른 series가 합쳐진다(D02 §10 금지) |
| 등록부 장애 | 503 | 허용(fail open): 장애 동안 cardinality가 무제한이다. 기존 series만 허용: cache가 없는 replica에서는 기존을 판정할 수 없다 |

## 결과

- cardinality 폭증 위험(D06 리스크 표)의 1차 방어가 생긴다. 사용자 ID 같은 dimension은 들어오지 못하고, tenant의 series 수가 묶인다.
- 제어 DB 부하: 신규 series와 10분 갱신. 부하 시험(100k series)에서 측정한다.

## Rollback

- ingress의 `Series`를 끄는 배포로 활성 상한만 해제할 수 있다. dimension 규칙은 그대로다.
- migration 00002 down은 등록부 테이블을 지운다.

## 재검토 조건

- 제어 DB 쓰기가 수집 경로 지연을 키울 때. 이때는 cache 간격을 늘리거나 별도 저장소를 쓴다.
- soft limit의 초과 폭이 문제될 때. 이때는 tenant별 advisory lock으로 등록을 직렬화한다.
- key당 활성 값 한도를 구현할 때.

## 증거

- `internal/telemetry/envelope`: `MetricStreams`가 envelope와 같은 stream ID를 낸다. `RemovePoints`가 지정 point만 지운다.
- `internal/quota`
  - 한도에서 기존 series 수용·신규 거절(같은 series의 point 모두), 거절 series는 등록하지 않음
  - 다른 replica가 등록한 series는 기존으로 판정, cache 10분과 갱신, 1시간 비활성 뒤 신규로 판정
  - 등록부 오류 전파, tenant override
  - 금지 dimension(point·resource, 대소문자)과 label 21개 거절, overrides `active_series` 검증
- `internal/ingest`
  - 금지 dimension·label 과다·series 초과 point만 거절하고 나머지는 append(partial success)
  - **redaction이 key를 지우기 전에 판정한다**(이 시험이 처음 실패하며 문제를 드러냈다).
  - 남은 point의 속성은 그대로다. 금지 dimension 값은 로그에 없다.
  - 등록부 장애는 503 + Retry-After이고 내부 문구를 노출하지 않는다. 그 밖의 등록부 오류는 500이다.
  - scope 속성의 금지 key, 긴 metric 이름, 등록 실패 시 자리 반환
- spec-reviewer 지적 반영: 잠금 안 DB 호출, 재시도 불가 오류의 503, soft limit 상한 명시, 등록 실패 시 count, scope 속성, rate token 순서 명시
- `internal/controldb` 통합 테스트(PostgreSQL): 등록·활성 수·Known, last_seen은 뒤로 가지 않음, tenant 분리(RLS), 정리는 해당 tenant만, tenant context 없으면 0행.
