# ADR 0030: metric label key당 활성 값 상한

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0029와 같은 위임). 근거는 §4
- 날짜: 2026-10-05 (제안·결정)
- 관련: F07, E03 · D02 §10 · ADR 0019, 0029

## 배경

D02 §10은 "key당 활성 값 100개"를 시작값으로 정한다. ADR 0029는 금지 dimension, label 수, 활성 series 상한까지만 구현하고 이 항목을 남겼다.

series 상한만 있으면 한 metric이 사용자 정의 경로 같은 고유값 dimension으로 tenant 한도를 혼자 다 쓸 수 있다.

## 결정

### 1. 판정

- **적용 대상:** 신규 series에만 적용한다. 이미 활성인 series(ADR 0029 §3)는 그대로 받는다.
- **신규 series 수락 조건:** 그 series의 point 속성마다 다음을 만족해야 한다.

  ```
  (metric, key)의 활성 값 수 + (이 값이 새 값이면 1) ≤ 상한(기본 100)
  ```

  - 하나라도 넘으면 그 series의 모든 point를 `label_value_limit_exceeded`로 거절한다(partial success).
  - 이 검사는 series 수 검사보다 먼저 한다.
- **이미 활성인 값:** 그 값으로 만든 다른 신규 series는 받는다.
- **같은 요청 안의 새 값:** 여러 series가 같은 새 값을 쓰면 자리는 하나만 쓴다.
- **활성 판정:** 최근 1시간이다.
  - 기존 series를 등록부에서 갱신할 때(10분 간격) 그 series의 값들도 함께 갱신한다. 그래야 쓰이는 값이 비활성으로 빠져 상한이 느슨해지지 않는다.

### 2. 저장 (migration 00003, 제어 DB)

- 테이블은 `metric_label_values(tenant_id, metric_name, label_key, value_hash, first_seen, last_seen)`다. RLS(ENABLE+FORCE)이고 `montracer_rw`에 최소 권한만 준다.
- **값은 저장하지 않는다.** redaction(ADR 0019)을 거친 뒤 값의 SHA-256 앞 128비트만 둔다.
- cache·잠금 밖 호출·2초 상한·실패 시 자리 반환·장애 503·정리(2시간)는 ADR 0029 §3과 같다.
- label key는 255자를 넘으면 `invalid_metric_label`로 거절한다(컬럼 제약과 같다).
- 기본값은 `MONTRACER_QUOTA_VALUES_PER_KEY`(100)이다.

### 3. 한계

- **soft limit:** 활성 값 수를 replica마다 15초 cache한다. 그래서 series 상한과 같은 방식으로 상한을 조금 넘을 수 있다(ADR 0029 §3).
- **값 해시의 사전 대입:** 낮은 entropy 값(작은 숫자 집합 등)은 해시만으로도 추측할 수 있다. 다만 값은 이미 redaction 정책을 통과한 dimension 값이고, 등록부는 tenant RLS와 앱 role 권한 안에 있다.
- **tenant·metric별 override:** 아직 없다. preview·dimension 정책 API(F07)와 함께 만든다.

### 4. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 채택 |
|---|---|---|
| key당 활성 값 상한 | **같은 형태의 공개 사례를 찾지 못했다.** Grafana Mimir는 tenant series 수(`max_global_series_per_user`)와 series당 label 이름 수(`max_label_names_per_series`)를 제한한다([GEM limits](https://grafana.com/docs/enterprise-metrics/latest/configure/config-gem/limits.md)). key별 distinct 값 수 상한은 그 목록에 없다 | 명세(D02 §10)가 직접 요구하므로 명세를 따른다. 집행 방식(신규만 거절, 기존 수용, partial success, 공유 등록부)은 ADR 0029의 Mimir 기반 결정을 그대로 쓴다 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 넘을 때 | 신규 series 거절 | 값을 `__other__`로 바꿔 수용: 서로 다른 series가 합쳐진다(D02 §10 금지) |
| 값 저장 | 해시 | 원값: PII 가능성이 있는 값을 제어 DB에 두게 된다 |

## Rollback

- `MONTRACER_QUOTA_VALUES_PER_KEY`를 크게 올린다.
- migration 00003 down은 등록부 테이블을 지운다.

## 증거

- `internal/quota`
  - 상한 2에서 세 번째 새 값의 신규 series는 거절하고, 기존 값의 신규 series와 기존 series는 받는다. 거절 series는 등록하지 않는다.
  - 같은 요청 안의 같은 새 값은 자리 하나만 쓴다.
  - 기존 series를 갱신하면 값의 활성 시각도 갱신한다(70분 뒤에도 상한 유지).
- `internal/controldb` 통합 테스트(PostgreSQL): (metric, key)별 활성 값 수, Known, tenant 분리(RLS), 정리는 해당 tenant만
