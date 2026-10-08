# ADR 0042: 서비스 상세(S02) RED 원천과 조회 보강

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 범위는 2026-10-07 사용자 선택: "UI + RED 백엔드 보강", "Vite dev proxy가 key 주입")
- Owner: FE · Backend
- 날짜: 2026-10-07 (제안·결정)
- 관련: F03, E03·E04, D05 §03·§05, D02 §07·§08·§12·§13·§19, ADR 0027, 0028, 0038, 0041

## 배경

D05 §05는 서비스 상세에 다음을 요구한다.

- 헤더(이름·owner·tier·계측 상태)
- 비샘플링 metric으로 계산한 요청량·오류율·p95(source badge 포함)
- endpoint 표(requests/s, error rate, p95, total time, total time 내림차순)
- 의존성·인스턴스·오류·프로파일·배포 탭
- 이를 위한 데이터 요청: GET service metadata와 POST query를 같은 범위로 부른다.

2026-10-07 현재 구현과 비교하면 다음이 부족하다.

1. 서비스 단건 조회가 없다. 목록(`GET /services`)만 있다.
2. RED에 쓸 metric 이름·형태가 정해져 있지 않다. seed는 counter `http.server.requests`만 보내서 p95를 낼 수 없다.
3. metric 조회에 histogram 합(total time)이 없다.
4. 범위 전체 요약 값(카드의 p95)을 낼 수 없다.
   - step 경계를 epoch에 맞추면 범위가 두 step으로 갈라진다.
   - 분위수는 step끼리 평균할 수 없다(계약 5).
5. 브라우저가 query-api를 부를 방법이 없다. bearer key만 받고, CORS 처리와 session 인증이 없다.
6. 의존성(service map)·인스턴스·배포·trace 검색 API가 없다.

## 결정

1. **RED 원천은 OTel HTTP semantic convention의 `http.server.request.duration`이다.**
   - 형태는 delta 또는 cumulative histogram이고 단위는 초다. 비샘플링 SDK metric이다(계약 5).
   - 요청 수는 histogram `count`, p95는 서버가 bucket을 병합해 계산한다(ADR 0025 §5).
   - **오류는 서버 응답 status 5xx다.** OTel 규칙상 server span은 4xx를 오류로 두지 않는다.
   - 서버에는 metric 간 연산이 없다(ADR 0027 §5). 그래서 오류율 = 5xx count / 전체 count를 UI가 status별 `count` 결과로 계산한다.
   - 어떤 status에도 값이 없는 step은 0이 아니라 null + 이유다. 요청이 0건이면 오류율은 `no_requests`다(계약 6).
   - 일부 status 값이 `no_data`가 아닌 이유로 비면, 합이 하한일 수 있어 partial로 표시한다.
   - 서비스 조건: `service.name` · `service.namespace` · `deployment.environment.name` eq. catalog 자연 키(D02 §08)와 같다.
2. **`GET /api/v1/services/{service_id}`** (query-api, `telemetry.read`)
   - 응답은 `{data: ServiceItem, meta}`이고, ServiceItem은 목록과 같은 형식이다.
   - 없는 서비스, 다른 tenant의 서비스, environment 제한 key가 볼 수 없는 서비스는 모두 같은 404다(D02 §19).
   - 형식이 틀린 id(소문자 UUID가 아님)는 존재와 무관하므로 400이다.
   - archived 서비스도 돌려준다. 과거 구간을 보려고 들어올 수 있기 때문이다.
   - 저장소 `controldb.ServiceStore.GetService`가 tenant(RLS)와 environment 제한을 mandatory predicate로 건다.
3. **metric 집계 `hist_sum`** (ADR 0027 집계표 추가): histogram 관측값의 합이다. endpoint 표의 total time 열에 쓴다. histogram이 아니면 `not_applicable`이다. 누락 window 판정(partial)은 `count`와 같다.
4. **범위 전체 한 점** (ADR 0027 §1 step 정렬의 예외)
   - 조건: `step_seconds`가 범위 길이(`to − from`)와 같고 `from`이 분 경계다.
   - 이때는 epoch 정렬 없이 범위를 그대로 한 step으로 집계한다.
   - 시작이 시간 경계가 아니면 1분 rollup을 읽는다(1시간 window가 범위 밖을 섞지 않게).
   - 이 규칙으로 카드의 요청량·오류율·p95와 endpoint 표가 "선택한 구간 전체" 값이 된다.
   - UI는 범위를 분 단위로 맞춘다(시작 내림, 끝 올림). 모든 카드·차트·표 query가 같은 범위와 같은 고정 시각(nowMs)을 쓴다. nowMs는 범위가 바뀌거나 새로고침할 때만 바뀐다.
   - 차트 step은 점 120개 이하가 되게 60초 배수로 잡는다. 1시간을 넘으면 1시간 배수로 올려 1시간 rollup을 읽는다.
   - 화면의 "집계 확정" 시각은 7개 query watermark 중 가장 이른 값이다. 하나라도 모르면 표시하지 않는다.
   - partial의 이유를 추측하지 않는다.
     - 범위 끝이 그 watermark보다 늦으면 "최근 구간 집계 중"이다.
     - 아니면 "빠진 window가 있어 값이 작을 수 있음"이다.
     - watermark를 모르면 "이유 확인할 수 없음"이다.
   - endpoint 행의 요청 수 값이 모두 비면 "요청 없음"으로 단정하지 않고 원래 이유(pending 등)를 보인다.
   - **D05 §05와의 차이:** D05는 metadata와 metric query를 병렬로 부르라고 한다. 하지만 metric 조건(name·namespace·environment)이 metadata에 있어 직렬로 부른다. metric을 service_id로 고를 수 있게 되면 병렬로 바꾼다(재검토 조건).
5. **seed에 duration histogram을 추가한다**
   - 대상: checkout 시나리오의 root 서비스.
   - 형태: OTel 권장 경계(0.005 … 10초), 속성 `http.route` · `http.request.method` · `http.response.status_code`, 분 단위 delta.
   - smoke oracle은 counter 합과 histogram count가 모두 요청 1,000·오류 20인지 확인한다.
   - 기존 counter `http.server.requests`는 유지한다.
6. **로컬 개발 인증은 Vite dev proxy가 key를 주입한다**(`apps/web/dev-proxy.ts`)
   - `/api`를 query-api(기본 `127.0.0.1:18080`)로 넘기면서 서버 쪽에서 seed API key(`.seed/demo.json`, 또는 `MONTRACER_DEV_API_TOKEN`)를 `Authorization`에 붙인다.
   - 브라우저가 보낸 `Authorization`·`Cookie`는 지운다. key는 브라우저로 가지 않는다(번들·저장소·응답 어디에도 없음). 같은 origin이라 CORS도 필요 없다.
   - 이 컴퓨터(loopback)에서 온 요청이고 `Sec-Fetch-Site`가 `cross-site`가 아닐 때만 넘긴다. 그 밖은 404다.
     - `vite --host`로 열어도 다른 기기가 seed key 권한으로 조회하지 못한다.
     - 다른 사이트가 시킨 요청이 tenant 조회 slot을 쓰지 못한다.
   - `vite preview`에는 붙이지 않는다.
   - 개발용 기본 조직 slug는 seed tenant 이름이다(`VITE_DEV_ORG`, ADR 0041 결정 10 보정).
   - 운영 인증(session cookie + CSRF, D02 §12)을 대신하지 않는다. build 결과물에는 이 코드가 들어가지 않는다.
7. **서버 상태는 작은 hook(`useRemote(queryKey, refreshToken, fetcher)`)으로 다룬다.** 아직 server-state library를 들이지 않는다.
   - 조건이 바뀌면 이전 요청을 `AbortController`로 취소한다. 늦게 온 응답이 최신 화면을 덮지 않는다(D05 §05 합격 기준).
   - **조회 조건(queryKey)이 바뀌면 이전 결과를 버리고 loading으로 돌아간다.** 다른 범위·서비스의 값이 새 조건의 값처럼 보이지 않게 하기 위해서다.
   - 같은 조건을 다시 부를 때(refreshToken만 바뀜)는 다시 불러오는 동안과 실패 뒤에도 마지막 성공 데이터와 시각을 유지한다(stale, D05 §03).
   - key에는 조직·서비스·query 전체를 넣는다. 결과는 컴포넌트 상태에만 두고 전역 store에 복사하지 않는다(D05 §04).
8. **API가 없는 탭은 비어 있는 척하지 않는다.** 의존성·인스턴스·오류·프로파일·배포 탭은 "아직 제공되지 않습니다"와 필요한 API를 말한다.
   - 헤더의 계측 상태(ADR 0038 §4에서 뺌)는 "확인할 수 없음", 사용자 필드(owner·tier)는 "미설정", monitor는 "판정 없음"이다.
   - endpoint에서 trace로 가는 이동은 trace 검색 API가 생길 때 붙인다.

## 외부 사례 근거

| 사례 | 내용 | 반영 |
|---|---|---|
| OTel HTTP metrics semconv ([opentelemetry.io/docs/specs/semconv/http/http-metrics](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/)) | `http.server.request.duration` histogram, 단위 `s`, 권장 경계 `[0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10]`, 속성 `http.route`·`http.request.method`·`http.response.status_code` | 결정 1·5 |
| OTel HTTP spans semconv ([opentelemetry.io/docs/specs/semconv/http/http-spans](https://opentelemetry.io/docs/specs/semconv/http/http-spans/)) | server span은 4xx를 오류로 두지 않고, 5xx는 오류 | 결정 1 오류 정의 |
| Prometheus HTTP API ([prometheus.io/docs/prometheus/latest/querying/api](https://prometheus.io/docs/prometheus/latest/querying/api/)) | 구간 전체 분위수는 instant query에서 `[구간]`의 bucket을 합친 뒤 `histogram_quantile`로 낸다(range query 점들의 평균이 아님) | 결정 4 |
| Vite `server.proxy` ([vite.dev/config/server-options](https://vite.dev/config/server-options)) | dev server가 API를 proxy해 같은 origin으로 개발 | 결정 6 |
| Datadog APM 서비스 페이지 ([docs.datadoghq.com/tracing/services/service_page](https://docs.datadoghq.com/tracing/services/service_page/)) | 서비스 단위 요청·오류·지연 그래프와 resource 목록 | 결정 1·3의 화면 구성 |

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. OTel histogram 하나로 RED (채택) | 요청·오류·p95·total time이 한 원천에서 같은 시각 기준으로 나옴. 업계 표준 이름 | SDK가 이 metric을 내지 않는 서비스는 빈 화면(안내 표시) |
| B. trace(span)에서 RED 계산 | 계측 하나로 충분 | 샘플링된 표본이라 계약 5 위반 |
| C. 오류율을 서버 연산으로 | UI 계산 없음 | ADR 0027 §5(metric 간 연산 없음)를 먼저 바꿔야 함 |
| D. 요약 카드에 step별 p95 평균 | 구현 쉬움 | 계약 5 위반(분위수 평균) |
| E. 브라우저에서 key 입력·저장 | proxy 불필요 | credential이 브라우저 저장소에 남음 |
| F. query-api에 CORS 허용 | 배포 형태와 비슷 | 개발 전용 예외가 서버 코드에 들어감, key 노출은 그대로 |

## 결과

- 이점
  - S02가 seed 데이터로 실제 RED를 보인다.
  - 누락·집계 중·요청 없음이 0과 구별된다.
  - 같은 범위를 공유해 카드와 표가 어긋나지 않는다.
- 비용
  - query 7개(차트 2, 요약 2, 표 3)가 나간다. tenant 동시 5·대기 20(ADR 0037) 안에 든다.
  - 최근 2분(watermark 이후)이 범위에 들면 요약 값은 항상 partial로 표시된다(정확한 표시).
  - 범위 시작은 대개 시간 경계가 아니다. 그래서 요약 query 5개는 1분 rollup을 읽는다.
    - 7일 범위면 `metric_1m` 7일치를 scan한다(서비스 조건으로 좁힌 행).
    - 차트는 1시간 배수 step으로 1시간 rollup을 읽는다.
    - 비용이 문제가 되면 요약을 시간 경계로 맞추고 앞뒤 부분 구간만 1분으로 읽는 분할을 검토한다(재검토 조건).
- 영향 받는 계약
  - 새 경로 `GET /api/v1/services/{service_id}`
  - metric 집계 `hist_sum`
  - step 정렬 예외(범위 = step)
  - **API 동작 변경:** 범위 = step이고 분 경계에서 시작하는 요청은 이전에는 정렬되어 두 점까지 나왔지만, 이제 한 점이다. 그 밖의 요청 결과는 바뀌지 않는다. 이 형태를 쓰던 호출자는 없었다.

## Rollback

- 새 경로와 `hist_sum`은 쓰는 곳이 S02뿐이다. web을 되돌린 뒤 서버에서 지우면 된다.
- seed histogram은 지워도 기존 counter oracle이 남는다.
- dev proxy는 개발 전용이다.

## 재검토 조건

- service map API가 생길 때: 의존성 탭, client/server 중복 없는 edge(D02 §08)
- trace 검색 API가 생길 때: endpoint → 실패 trace 3클릭(D05 §05 합격)
- 배포 이벤트 API가 생길 때: 배포 marker와 version 비교
- metric 간 연산을 서버에 넣을 때: 결정 1의 오류율 계산 위치
- metric을 service_id로 고를 수 있게 될 때: metadata와 metric query 병렬 호출(D05 §05)
- 긴 범위 요약의 1분 rollup scan 비용이 실측에서 문제가 될 때: 결정 4의 시간 경계 분할
- session 인증이 생길 때: 결정 6의 dev proxy를 cookie 방식으로 바꾼다
- server-state library를 들일 때: 결정 7

## 증거

- Go
  - `internal/query/services_test.go` `TestGetService`(200·404·400·401, principal 범위 전달)
  - `internal/controldb/services_integration_test.go`(tenant·environment 격리, archived, 없는 id)
  - `tests/isolation`: B가 A의 service_id로 단건 조회(X-Tenant-ID 위조 포함)하면 없는 id와 같은 404·같은 오류 code
  - `internal/query/metrics_test.go` `TestMetricHistogramSum`, `TestMetricWholeRangeSingleStep`
  - `scripts/dev/demo/demo_test.go`(histogram count·bucket·sum)
  - smoke histogram oracle(1분 step, 그리고 범위 전체 한 점: series마다 점 1개이고 요청 1,000·오류 20)
- web
  - `red.test.ts`(오류율 정확히 2%, null·pending·partial, 표 정렬)
  - `ServiceDetail.test.tsx`(헤더 미설정·판정 없음, 카드 값, 빈 step은 "데이터 없음", query 7개 같은 범위·조건, 404, metric 없음 안내, 429 Retry-After·request ID, API 없는 탭, query별 503은 그 자리만 "확인할 수 없음")
  - `useRemote.test.tsx`(늦은 응답 무시, 조건 변경 시 이전 결과 버림, 같은 조건 새로고침 시 stale 유지)
  - `ServiceDetail.test.tsx`의 범위 변경 시험은 이전 동작(조건이 바뀌어도 값 유지)이면 실패함을 확인
  - `dev-proxy.test.ts`(브라우저 Authorization·Cookie 제거, key 주입, loopback·same-site만)
- 수동: mock query-api로 dev server 확인(목록 → 상세, 빈 step 끊김, 집계 중 음영, 404)
  - 로컬 stack E2E는 이 환경에서 `.env` 생성이 막혀 하지 못했다. 통합은 CI의 `go integration`이 확인한다.

## 변경 이력

- 2026-10-07 (ADR 0043): trace 검색이 생겨 리소스 표에 "오류 trace 보기"·"이 서비스의 trace"(S04) 링크를 달았다. endpoint(route)별 trace 이동은 S04에 route 조건이 생기면 붙인다.
- 2026-10-08 (PS-0009): query 하나가 실패하면 그 카드·차트가 "받은 metric 없음", 리소스 표가 빈 표로 보이던 결함을 고쳤다(D05 §03 실패 > empty, 계약 6). S01 Overview(ADR 0048)와 같은 규칙이다. 실패한 카드 값은 `query_failed`("확인할 수 없음(조회 실패)"), 차트는 "차트를 확인할 수 없음(조회 실패)"이다. 표 query(route count·p95·hist_sum) 중 하나라도 실패하면 표 대신 실패와 "endpoint가 없다는 뜻이 아닙니다"를 보인다. 위쪽 알림은 카드 query 실패(개요 탭)만, 표 실패는 표 자리에서만 보여 같은 실패를 두 번 보이지 않는다. 어느 query든 실패하면 "metric을 받지 못했습니다" 안내로 단정하지 않는다.
