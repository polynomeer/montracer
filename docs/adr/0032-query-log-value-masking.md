# ADR 0032: 조회 값이 ClickHouse query_log·로그에 남지 않게 하기

- 상태: 제안
- Owner: Backend lead (query), SRE lead (ClickHouse 설정)
- 날짜: 2026-10-06 (제안)
- 관련: D04 §10, §11 · D02 §15 · ADR 0018, 0023 · RB02, RB03

## 배경

D04 §10은 플랫폼 로그에 원시 filter literal을 남기지 않도록 요구한다. 그런데 `internal/telemetrystore`의 조회에 쓰는 사용자 값이 ClickHouse `system.query_log.query`에 그대로 남는다. 대상 값은 trace_id, metric 이름, label key·value, environment 목록이다. 지금까지 RB02·RB03은 "`query` 컬럼을 읽지 말라"는 운영 규칙으로 막았다.

이 ADR을 쓰며 확인한 사실은 두 가지다 (§증거).

1. **clickhouse-go 위치 인자(`$1`)는 client 측 binding이다.** `bindNumeric`(clickhouse-go v2.48.0 `bind.go`)이 값을 SQL literal로 바꿔 query 문자열에 끼운 뒤 보낸다. 그래서 서버 query_log에 남는다. driver의 debug 로그(`sendQuery`의 `"sending query"`)에도 같은 문자열이 찍힌다.
2. **서버 측 query parameter(`{name:Type}`)로 바꿔도 query_log에는 값이 남는다.** ClickHouse 26.8은 parameter를 치환한 AST를 query_log에 기록한다. String은 `'값'`, 다른 type은 `_CAST('값', 'Type')` 형태다. "서버 측 binding으로 바꾸면 해결된다"는 처음 가정은 틀렸다.

## 결정

두 겹으로 막는다.

### 1. 서버 측 query parameter로 binding한다 (`internal/telemetrystore`)

- 모든 사용자 값은 `{name:Type}` placeholder와 `clickhouse.Named(name, value)`로 보낸다. 이 경로는 clickhouse-go가 `WithParameters`와 같은 protocol parameter로 보낸다.
- 대상은 trace 조회(tenant, trace_id, service_id, 시간, limit), metric 조회(metric, label key·value, environment `Array(String)`, step), watermark·coverage 조회다.
- client가 보내는 SQL 문자열에는 고정 template과 parameter 이름만 남는다. 그래서 우리 서비스 쪽 driver 로그·오류 문맥에는 값이 없다.
- 시각은 초 단위로 내려 `DateTime('UTC')`로 보낸다. 이전 client binding(`toDateTime('…')`)도 초 단위였으므로 tenant·시간·expires_at mandatory predicate의 의미는 같다.
- row policy용 `SQL_montracer_tenant` 설정은 바꾸지 않는다 (ADR 0018).
- parameter 프로토콜을 지원하지 않는 서버(22.x 미만)에서는 driver가 값을 치환하지 못한다. 그러면 서버가 parameter 누락 오류를 낸다. 값을 문자열에 끼우는 쪽으로 조용히 되돌아가지 않는다(fail closed).

### 2. ClickHouse `query_masking_rules`로 문자열 literal을 `'?'`로 바꾼다

- 서버 설정 `config.d/query-masking.xml`에 정규식 규칙을 하나 둔다: `'(?:[^'\\]|\\.)*'` → `'?'`.
- ClickHouse는 이 규칙을 저장 전에 적용한다. 대상은 query_log, text_log, processes, 서버 로그다. 그래서 치환된 parameter 값과, 혹시 남은 client binding 값이 모두 가려진다.
- 숫자(시각 epoch, limit, step)와 SQL 구조는 남는다. 느린 query 진단(RB02)에는 query_id, `normalized_query_hash`, 읽은 행·byte, 코드의 template으로 충분하다.
- 규칙은 사용자별이 아니라 서버 전체에 적용된다. production 템플릿에도 같은 파일을 둔다(ADR 0018의 access_control 설정과 같은 방식).

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A (채택) 서버 측 parameter + 문자열 literal masking | query_log·서버 로그·우리 서비스 로그 모두에서 값이 빠진다. query_log의 지연·자원 metadata는 유지된다 | template의 문자열 상수(`'UTC'`, JSON key 등)도 `'?'`가 된다. 따옴표로 값을 인용한 서버 오류 메시지도 가려진다 |
| B 서버 측 parameter만 | 코드 변경만 필요하다 | query_log에 값이 그대로 남는다(§증거 2). 요구를 충족하지 못한다 |
| C `_CAST('…'` 형태만 masking | template 상수가 남는다 | String parameter는 `_CAST` 없이 `'값'`으로 기록된다. 그래서 metric 이름·label 값을 놓친다 |
| D query 계정의 `log_queries=0` | 값이 query_log에 아예 남지 않는다 | RB02의 느린 query 분석, RB03의 접근 감사 근거(query_id·설정·행 수)를 잃는다 |
| E 운영 규칙 유지 ("query 컬럼을 읽지 말라") | 변경이 없다 | 저장은 계속된다. 관리자 계정·backup·export로 새어 나간다. D04 §10을 위반한다 |

## 결과

- 이점: RB02·RB03의 "`query` 컬럼 금지" 예외 규칙이 필요 없어진다(적용 이전 행 제외, §Rollout). 이후 추가되는 조회도 기본으로 가려진다.
- 비용 / 운영 부담:
  - query_log·서버 로그에서 문자열 상수를 볼 수 없다. 재현이 필요하면 query_id로 코드 template을 찾고, 값은 요청 측(API 감사 로그의 권한 범위 안)에서 얻는다.
  - masking 정규식은 모든 query·로그 문장에 적용되어 CPU 비용이 든다. 아직 측정하지 않았다. 부하 시험(E10)에서 잰다.
- 영향 받는 계약:
  - API·schema는 바뀌지 않는다.
  - 통합 테스트 `TestQueryLogHasNoBoundValues`를 추가했다. trace 조회, label filter·group_by·environment 제한이 있는 metric 조회, watermark 조회를 한 뒤 query_log 행 전체(모든 컬럼)에 값이 없음을 확인한다.
  - CI ClickHouse도 같은 `config.d`를 mount하므로 규칙이 빠지면 이 시험이 실패한다.
- 남은 위험: ClickHouse 오류 문구는 값을 따옴표 없이 인용하기도 한다(예: `Cannot parse uuid <값>`, §증거). 이런 값은 masking 규칙에 걸리지 않고 query_log `exception`과 서버 로그에 남는다.
  - 완화: telemetrystore는 trace_id·UUID·label 길이를 SQL 전에 검증하므로 parse 오류가 날 입력은 서버에 가지 않는다.
  - RB02·RB03은 `exception` 문구 대신 `exception_code`만 쓰도록 한다. 오류 문구 masking은 재검토 조건에 둔다.
- 범위 밖:
  - `internal/rollup`과 `tests/load/chlayout`의 위치 인자는 시각·합성 fixture 값만 binding하고 사용자 값이 없다. 그대로 둔다. masking 규칙은 이 둘에도 적용된다.
  - `system.query_log`의 보존 기간(TTL)은 정하지 않았다. 보존·삭제 정책(D04 §03)에서 따로 정한다.

## Rollout

1. ClickHouse 설정에 `query-masking.xml`을 배포하고 서버를 재시작한다. masking 규칙은 기동 시 읽는다.
2. query service를 배포한다. 순서는 상관없다. 각 단계가 독립적으로 값을 가린다.
3. 배포 이전에 쌓인 query_log 행에는 값이 남아 있다. 기존 클러스터는 배포 후 `TRUNCATE TABLE system.query_log`로 지운다. 아직 production은 없으므로 로컬·CI 환경만 해당한다. 그 전까지 RB02·RB03은 이전 행의 `query` 컬럼을 읽지 않도록 안내한다.

## Rollback

- 설정 파일을 지우고 재시작하면 masking이 꺼진다. 다만 그 사이 기록된 행에는 값이 남는다.
- 코드는 revert로 위치 인자로 되돌릴 수 있지만 권장하지 않는다. masking이 있으면 query_log는 계속 가려지지만, 우리 서비스의 driver 로그에 값이 다시 생긴다.
- 지워진(가려진) 과거 query_log 값은 복구할 수 없다. 의도한 결과다.

## 재검토 조건

- ClickHouse가 query_log에 parameter를 치환하지 않는 설정을 제공하면 masking 범위를 줄인다.
- masking 비용이 부하 시험에서 query p95의 1%를 넘으면 다시 연다.
- 사용자 값이 들어간 오류 문구가 query_log·서버 로그에서 발견되면 다시 연다(입력 검증 보강 또는 오류 문구 masking 규칙).
- 운영 진단에 문자열 상수가 꼭 필요해지면 다시 연다. 예를 들어 template 상수를 별도 이름으로 남기는 방식을 검토한다.

## 외부 사례 근거

- **Datadog Database Monitoring**: query bind parameter를 모두 obfuscation하고 literal을 `?`로 바꾼 뒤 intake로 보낸다. 근거 <https://docs.datadoghq.com/database_monitoring/data_collected/>
- **Google Cloud SQL Query Insights**: 정규화된 query만 저장·표시한다. 상수는 `?` 또는 `$1`로 바뀐다. 근거 <https://docs.cloud.google.com/sql/docs/postgres/using-query-insights>
- **OpenTelemetry semantic conventions**: `db.query.text`는 sanitization 시 모든 literal(String, Numeric, Date 등)을 placeholder로 바꿔야 한다(SHOULD). parameterized query는 값이 따로 가므로 그대로 둔다. 근거 <https://opentelemetry.io/docs/specs/semconv/database/database-spans/>
- **ClickHouse**: `query_masking_rules`는 정규식 규칙을 query와 로그 문장에 적용한다. 서버 로그와 system.query_log·text_log·processes에 저장하기 전이다. 근거 <https://clickhouse.com/docs/cloud/guides/data-masking>

세 서비스의 공통점은 "저장되는 query 문장에서 literal을 placeholder로 바꾼다"이다. A는 이를 ClickHouse의 자체 기능으로 따른다. OTel의 "parameterized query는 그대로 둔다"는 parameter 값이 query 문장 밖에 남는다는 전제다. ClickHouse는 이 전제를 지키지 않는다(§증거 2). 그래서 parameter만 쓰는 B는 채택하지 않았다.

## 증거

`internal/telemetrystore/querylog_integration_test.go`를 CI와 같은 ClickHouse 이미지(`deploy/compose/versions.env`, 26.8)와 `config.d`로 실행했다.

| 코드 | masking 규칙 | 결과 |
|---|---|---|
| 서버 측 parameter | 없음 | 실패. trace 조회 query에 `unhex('<trace_id>')`, `_CAST('<tenant>', 'UUID')`가 남는다. metric 조회 query에 `metric_name = 'it.metric'`, label key·value, environment 배열이 남는다 |
| 서버 측 parameter | `_CAST('…'`만 | 실패. String parameter(metric 이름, label 값)는 `_CAST` 없이 기록된다 |
| 서버 측 parameter | 문자열 literal 전체 | 통과. 기록 예: `metric_name = '?'`, `window_start >= _CAST(1791213840, '?')` |
| 이전 위치 인자 | 문자열 literal 전체 | 통과. masking만으로도 query_log는 가려진다. 다만 client 측 SQL 문자열(driver 로그)에는 값이 남는다 |

masking 적용 확인: 관리자 계정의 `SELECT 'rb03-masking-check'`는 query_log에 `SELECT '?'`로 남는다. 같은 설정에서 `SELECT toUUID('secret-value-xyz')`의 오류는 query 부분이 `toUUID('?')`로 가려지지만, 앞부분 `Cannot parse uuid secret-value-xyz`에는 값이 남는다.

client 측 binding 근거: clickhouse-go v2.48.0 `bind.go`의 `bindNumeric`(값을 `format()`으로 literal로 바꿔 문자열에 끼움), `query_parameters.go`의 `bindQueryOrAppendParameters`(`{name:Type}`와 Named 인자일 때만 protocol parameter로 보냄), `conn_send_query.go`의 `"sending query"` debug 로그.
