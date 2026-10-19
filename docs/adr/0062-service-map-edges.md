# ADR 0062: Service map edge — span 쌍 1분 rollup과 조회 API (S03 단계 A)

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-19 다음 작업 선택 "S03 Service Map")
- Owner: Data/API lead
- 날짜: 2026-10-19 (제안·결정)
- 관련: F03, E03·E04, D02 §08(Map 산출 규칙·이중 계수), §10(`service_edges_1m` 30일), §13(`POST /service-map/query`, 500 nodes·2,000 edges), D05 §07(S03) · ADR 0018, 0026, 0038, 0039, 0059

## 배경

- S03 Map의 원천이 필요하다. D02 §08은 다음을 정한다.
  - edge는 client/server(producer/consumer) span의 짝으로 만들고, parent-child 연결이 우선이다.
  - 이름으로 추정한 edge는 `estimated`다.
  - DB·외부 SaaS는 external node로 묶고 host/IP 전체를 노출하지 않는다.
  - 요청 수는 client 기준이고, client가 없으면 server로 센다. 같은 RPC를 두 번 세지 않는다.
  - messaging은 처리 시간과 큐 대기를 나눈다.
  - 계산은 1분 window다. 권한 밖 서비스는 map에서도 없앤다.
- 명세가 정하지 않은 것: 짝을 맞추는 위치(수집 stream·주기 batch·조회 시점), 저장 형태와 늦은 span 처리, 외부 상대를 묶는 key, job 계정 권한, 응답 형태(`omitted_count`의 모양, estimated의 수).
- D02 §08의 "SDK dependency metric 우선"은 아직 할 수 없다. 수집하는 dependency metric이 없다. 지금은 span만 있다.

## 결정

### 1. 짝은 주기 batch job이 ClickHouse에서 맞춘다 — `service_edges_1m`

- worker 역할 `servicemap`(`internal/servicemap`)이 30초마다 닫힌 1분 window를 계산한다.
  - watermark·10분 재계산·따라잡기 1시간·tenant별 진행은 metric rollup과 같은 규칙이다(`rollup.Planner`로 공유, ADR 0026).
  - cluster에 하나만 켠다.
- 짝은 ClickHouse join으로 맞춘다. 같은 span의 merge 전 중복 행은 `LIMIT 1 BY`로 한 번만 센다(seed 2회 smoke가 확인한다). 한 번에 읽는 범위는 10분이다(join 메모리 상한).
  - **client 기준:** client·producer span의 시작 시각이 window다. 그 span을 부모로 둔 server·consumer span을 [−1분, +5분] 안에서 찾는다.
    - 서비스가 다르면 source → 그 서비스(observed)다.
    - 요청 수·오류(client 또는 server 오류)·latency(client가 본 시간)는 client 쪽에서 센다.
  - **fallback:** client span 없이 다른 서비스의 internal·server span 아래에서 시작한 server·consumer span을 server 쪽으로 센다. 부모가 client·producer면 이미 셌으므로 뺀다.
  - **messaging:** latency는 consumer 처리 시간이다. 큐 대기(consumer 시작 − producer 끝)는 따로 histogram에 담는다. 짝이 없는 producer의 latency는 publish 시간이다.
- histogram은 OTel SDK 기본 explicit bucket(ms)이다. 조회는 bucket을 병합한 분위수다(계약 5).
- 결과는 `(tenant, window, source, destination, connection)` 행이다. `service_edges_1m`에 30일 보관한다(D02 §10).

### 2. 짝 없는 client span의 상대는 속성으로 묶고, host 전체를 남기지 않는다

다음 순서로 정한다. 정할 수 없는 span과 자기 호출은 버리고 수를 지표로 센다(`montracer_servicemap_pairs_dropped_total`).

| 속성 | destination | node |
|---|---|---|
| `db.system.name`(구 `db.system`) + `db.namespace`(구 `db.name`) | `db:<system>[/<namespace>]` | database |
| `messaging.system` | `queue:<system>` (destination 이름은 임시 큐가 많아 쓰지 않음) | queue |
| `peer.service` | `peer:<이름>` | 조회 때 결정(§4) |
| host(`server.address`→`net.peer.name`→`url.full`·`http.url`의 domain) | IP → `host:ip` 한 node. 점 없는 이름·`*.svc`·`*.cluster.local` → `peer:<첫 label>`. 그 밖 → `host:<등록 domain(eTLD+1)>` | external / peer |

- 외부 host는 등록 domain까지만 둔다(`api.eu.stripe.com` → `stripe.com`). IP는 하나로 묶는다(D02 §08).
- cluster 내부 이름의 첫 label은 Kubernetes Service 이름이다. 서비스 이름 후보로 쓴다.

### 3. window는 통째로 같은 revision으로 쓰고, 조회는 window의 최대 revision만 읽는다

늦은 server span이 짝을 찾으면 같은 호출이 `peer:payment` 행에서 `payment` 서비스 행으로 옮겨 간다. key가 바뀌므로 ReplacingMergeTree가 옛 행을 지우지 않는다. 그래서 다음처럼 한다.

- job은 내용이 바뀐 (tenant, window)의 **모든 행을 같은 revision으로** 다시 쓴다(내용 해시가 같으면 쓰지 않는다).
- 조회는 `(window_start, revision) IN (window별 max(revision))`인 행만 합친다. 옛 key 행을 섞어 두 번 세지 않는다.
- insert token은 내용 기반이다. 재시도해도 한 번만 들어간다(metric rollup과 같다).

### 4. 조회 API — catalog로 이름을 붙이고, 볼 수 없는 것은 흔적 없이 뺀다

`POST /api/v1/service-map/query {range(최대 7일), environment?, service_ids?(≤100)}`

- 범위 시작은 분 경계로 내린다. `meta.sampled=true`(span 표본), `resolution_seconds=60`, `watermark`는 그 tenant의 마지막 window 끝이다.
- **권한:**
  - environment 제한 key는 허용 서비스 집합을 source의 mandatory predicate로 넘긴다(ADR 0039와 같은 방식).
  - 모든 endpoint는 `ServicesByID`(같은 범위 규칙)로 다시 거른다. 볼 수 없거나 catalog에 없는 서비스에 닿는 edge는 통째로 뺀다(fail closed, 수도 남기지 않는다).
- **environment:**
  - 서로 다른 environment의 두 서비스 사이 edge는 숨기고 `cross_environment_edges_hidden` 경고를 붙인다(D02 §08 "environment 내에서만 연결").
  - 외부 node는 source environment마다 따로다(`db:postgresql@prod`).
- **estimated:** `peer:<이름>`이 source와 같은 environment에서 이름이 같은(대소문자 무시) 서비스 **하나**와 맞으면 그 서비스로 가는 `confidence=estimated` edge다.
  - `requests`·`errors`·`latency`는 null이다(D05 §07 "추정 edge에 정확한 호출 수를 붙이지 않는다").
  - 맞는 서비스가 없거나 둘 이상이면 external node다.
- **상한:** traffic이 많은 node부터 500개, 그 사이 edge를 요청 수 순으로 2,000개 남긴다. 뺀 수는 `omitted_count: {nodes, edges}`다.
  - D02 §13은 `omitted_count` 하나를 말한다. node와 edge의 의미가 달라 둘로 나눴다.
  - 저장소 읽기 상한(9,999 집계 행)에 걸리면 `edge_rows_truncated` 경고를 붙인다.
- histogram 경계가 다른 window를 합쳐야 하면 latency를 null로 두고 `latency_bounds_mismatch` 경고를 붙인다.
- flag `MONTRACER_SERVICE_MAP_API_ENABLED`(기본 켜짐)가 false면 404다.

### 5. job 전용 ClickHouse 계정

- `montracer_ch_servicemap` role이 받는 권한:
  - spans_local의 짝 계산 컬럼 11개만 SELECT(payload·name·received_at 제외)한다. 전 tenant row policy를 명시한다.
  - `service_edges_1m`에 INSERT하고, 진행 컬럼 3개만 SELECT한다.
- 기동 때 확인: span payload·log·metric을 읽을 수 있으면 거부한다(`ErrTooPrivileged`).
- metric rollup 계정에 span 읽기를 더하지 않는다. ADR 0026 §3이 "rollup은 span을 읽지 못한다"를 기동 검사로 지킨다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| **A. 주기 batch(ClickHouse join) → 1분 테이블 (채택)** | 이미 저장한 dedup span으로 계산하고, 늦은 span을 10분 재계산으로 흡수한다. 조회는 작은 집계를 읽는다. Jaeger spark-dependencies와 같은 방식 | 짝을 맞출 때 span join 비용(10분 chunk)이 든다. window가 2분 이상 늦게 닫힌다 |
| B. 수집 stream에서 짝 맞추기(Tempo metrics-generator 방식) | 지연이 짧다. 표본 전 span으로 셀 수 있다 | worker partition이 trace 단위가 아니다(같은 trace span이 여러 worker로 간다). 짝 대기 상태를 따로 공유해야 하고, 재시작 시 대기 중인 짝을 잃는다 |
| C. 조회 시점 join | 저장이 없다 | 24시간·7일 범위 join은 예산(5초·10GB, ADR 0018)을 넘는다. span 보존보다 긴 30일 map이 안 된다 |
| D. metric rollup 계정·job에 합치기 | 계정·역할이 하나 줄어든다 | rollup 계정이 span을 읽게 된다(ADR 0026 §3 기동 검사와 충돌). 한쪽 장애가 다른 쪽을 멈춘다 |
| 외부 상대: host 전체 저장 | 정확하다 | D02 §08 위반(host/IP 노출). IP마다 node가 생긴다 |
| estimated에도 수 표시 | 정보가 많다 | D05 §07 위반. 이름만 맞춘 관계에 정확해 보이는 수를 붙인다 |

## 결과

- 이점:
  - map은 1분 해상도로 30일을 본다. 늦은 span이 짝을 찾으면 같은 호출이 옮겨 가고 두 번 세지 않는다.
  - 권한 밖 서비스와 host 원문이 응답에 나타나지 않는다.
- 비용 / 운영 부담:
  - 새 ClickHouse 계정·worker 역할·경보(`MontracerServiceMapRollupStalled`, ticket — 조사 화면 하나라 page가 아니다)·RB01 절이 생긴다.
  - span join 비용은 부하 시험(10k spans/s)에서 잰다.
- 영향 받는 계약:
  - OpenAPI `queryServiceMap` 추가(additive)
  - migration 00006
  - 격리 suite `TestServiceMapAcrossTenants`
  - smoke 5b(seed 2회 → checkout→payment 1,000/20, database→postgresql 1,000)
- 남은 것:
  - S03 화면(단계 B)
  - SDK dependency metric 우선 사용(D02 §08)
  - 5분·1시간 rollup(지금은 1분 행을 7일까지 합친다)
  - span link 기반 관계(batch consumer)
  - 서비스 edge가 없는 고립 서비스 node
  - team·namespace 필터
  - edge backfill job

## Rollback

- API: `MONTRACER_SERVICE_MAP_API_ENABLED=false` → 404.
- job: worker 역할에서 `servicemap`을 뺀다. 원본 수집·metric rollup과 독립이다.
- migration down은 테이블·정책·권한을 지운다(role은 남긴다). 집계만 잃고 원본 span은 남는다. 다시 올리면 job이 최근 1시간부터 다시 계산한다(그 이전은 backfill이 없어 빈다).

## 재검토 조건

- 부하 시험에서 주기 실행 시간이 주기(30초)를 넘거나, ClickHouse 메모리 초과가 나면 다음을 검토한다.
  - chunk 축소
  - 짝 후보 범위(+5분) 축소
  - 수집 stream 방식(B)과 trace 단위 partition
- SDK·Collector가 dependency metric을 보내기 시작하면 요청 수·오류·latency의 원천을 그쪽으로 바꾼다(D02 §08).
- `pairs_dropped`가 client span의 상당 부분이면 상대 속성 규칙을 넓힌다(`rpc.service` 등).

## 근거 (빅테크 사례)

- **Grafana Tempo service graphs** — https://grafana.com/docs/tempo/latest/metrics-generator/service_graphs/
  - client·server span 짝으로 edge를 만든다.
  - 계측되지 않은 상대는 `peer.service`·`db.name`·`server.address` 등으로 virtual node를 만든다.
  - messaging은 producer·consumer 짝과 대기 시간을 따로 낸다.
  - §1(짝 규칙), §2(상대 속성), messaging 분리의 근거다. stream 방식(B)을 버린 이유는 위 표에 있다.
- **Jaeger spark-dependencies** — https://github.com/jaegertracing/spark-dependencies
  - 저장된 span의 parent-child 관계로 서비스 의존 링크를 주기 batch로 계산해 따로 저장한다.
  - §1 batch 방식의 근거다.
- **AWS X-Ray service map** — https://docs.aws.amazon.com/xray/latest/devguide/xray-console-servicemap.html
  - 계측되지 않은 downstream(DB·외부 HTTP)을 호출 쪽 subsegment로 추정한 node(inferred)로 그린다.
  - map은 표본 trace로 만든다.
  - §2, `meta.sampled`의 근거다.
- **Datadog Service Map / inferred services** — https://docs.datadoghq.com/tracing/services/services_map/
  - DB·queue·외부 API를 client span 속성으로 추정한 서비스로 보이고, 환경(env)별로 map을 나눈다.
  - §2, §4 environment 분리의 근거다.
- **OpenTelemetry semantic conventions** — `peer.service`, `server.address`, `db.system.name`, `db.namespace`, `messaging.system`
  - https://opentelemetry.io/docs/specs/semconv/
  - §2의 속성 이름이다. 구 이름(`db.system`, `db.name`, `net.peer.name`, `http.url`)도 읽는다.

## 증거

- 단위:
  - `internal/servicemap`: 상대 분류, bucket 변환, window 해시, revision, chunk·tenant 범위
  - `internal/query/service_map_test.go`: estimated·environment·상한·경계 불일치·거절·flag off, OpenAPI strict 계약
- 통합(CI):
  - `TestEdgeRollupEndToEnd`: 중복 행, fallback, messaging 대기, IP 숨김, 늦은 span revision 교체, 재시작, tenant 격리
  - `TestServiceMapAccountIsNarrow`
  - `ServicesByID`
  - `TestServiceMapAcrossTenants`
- smoke 5b.
