# ADR 0023: 플랫폼 운영 지표·경보·runbook (ingress·worker·query)

- 상태: 승인 (결정 위임)
- Owner: SRE lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021·0022와 같은 위임). 근거는 §6
- 날짜: 2026-10-05 (제안·결정)
- 관련: G1 Gate(Q3·Q5) · D04 §10~11 · D02 §21 · D06 §02 DoD · ADR 0014, 0020 §7, 0021

## 배경

D04 §10이 요구하는 것은 다음과 같다.

- 운영용 Prometheus와 외부 heartbeat를 고객 경로와 **별도 실패 영역**에 둔다.
- 단계별 필수 지표와 초기 경보 조건을 둔다.
- platform metric label에 telemetry body·user_id를 넣지 않는다. tenant label은 bounded로 둔다.

ADR 0020 §7과 0021은 수집 지표와 RB01 runbook을 G1 Gate 차단 항목으로 남겼다. 지금까지 ingress·worker 결과는 구조화 로그에만 남았다.

## 결정

### 1. 노출 방식

- 라이브러리는 `github.com/prometheus/client_golang` v1.24.1을 쓴다. 각 binary가 **별도 listener**(`MONTRACER_METRICS_ADDR`, 기본 `:9464`)에서 `/metrics`·`/healthz`를 노출하고, 운영용 Prometheus가 pull한다.
  - 고객 트래픽 listener와 분리했다. 그래서 외부 load balancer에 노출되지 않고, 고객 경로가 과부하일 때도 지표를 읽을 수 있다.
- 전역 `DefaultRegisterer` 대신 binary별 registry를 쓴다. 여기에 Go runtime·process collector를 포함한다.
- 지표 listener가 죽으면 프로세스를 내린다. 지표 없이 조용히 도는 상태를 만들지 않기 위해서다.
- **도메인 패키지는 Prometheus를 import하지 않는다.**
  - `ingest.Observer`, `pipeline.Observer`, `httpapi.Observe`를 각 패키지에 둔다. 구현은 `internal/opsmetrics`에 둔다(ADR 0014 §1과 같은 의존 방향).
  - Observer가 nil이면 지표를 내보내지 않는다. 그래서 시험과 라이브러리 사용에 영향이 없다.

### 2. 지표 (D04 §10 표 대응)

| 단계 | 지표 | label |
|---|---|---|
| Ingress | `montracer_ingress_requests_total`, `montracer_ingress_records_total`(accepted·rejected), `montracer_ingress_request_duration_seconds`, `montracer_ingress_kafka_append_duration_seconds` | signal, status_class, outcome, reason |
| Worker | `montracer_worker_records_total`(stored·duplicate·quarantined), `montracer_worker_conflicts_total`, `montracer_worker_store_duration_seconds`, `montracer_worker_oldest_record_age_seconds`, `montracer_worker_sink_errors_total`, `montracer_worker_offset_commits_total` | signal, outcome, reason, kind |
| Query | `montracer_query_requests_total`, `montracer_query_request_duration_seconds` | route(등록 pattern), status_class |

- **stage 회계 (D02 §21):** ingress `accepted`와 worker `records`(stored + duplicate + quarantined)는 **같은 단위**, 곧 Kafka record다. 처리율 비교와 회계 대사에 그대로 쓴다. 단위 테스트로 불변식을 고정했다.
- **label은 고정 enum만 쓴다.**
  - tenant, user, trace·span ID, key, URL, 원문은 label로 쓰지 않는다.
  - route는 등록된 pattern이고, 등록되지 않은 경로는 `unmatched` 하나로 묶는다.
  - tenant별 건수는 usage 원장(D04 §08)이 맡는다. 단위 테스트가 허용 label 밖의 label을 거부한다.
- **아직 없는 것**
  - Kafka broker 쪽 지표(ISR, disk, consumer lag in records)는 Kafka exporter로 수집한다. 이 저장소의 binary가 내보내지 않는다.
  - Store 쪽 part 수·replication lag은 ClickHouse 내장 Prometheus endpoint로 수집한다.
  - 둘 다 배포 템플릿에서 scrape를 설정한다.

### 3. 경보 (D04 §10 초기 경보 조건)

규칙은 `deploy/prometheus/rules/montracer.rules.yml`에 둔다(9개).

| 경보 | 조건 | 심각도 |
|---|---|---|
| MontracerIngressErrorRateHigh | 5xx > 0.5% 5분 (D04 §10) | page |
| MontracerIngressKafkaAppendFailing | append 실패 5분 지속 | page |
| MontracerWorkerFallingBehind | 처리율 < 승인률×0.99 10분 (D04 §10). worker 지표가 없으면 0으로 본다 | page |
| MontracerPipelineFreshnessLag | oldest age > 5분 (D04 §11 RB01) | page |
| MontracerWorkerStoreFailing | 일시 저장 실패 5분 | warning |
| MontracerWorkerRowsRejected | sink_rejected > 0 (15분) | ticket |
| MontracerWorkerQuarantineRatioHigh | quarantine > 1% 10분 | warning |
| MontracerWorkerCommitFailing | commit 실패 > 0 (10분) | warning |
| MontracerQueryErrorRateHigh | 5xx > 1% 10분 (synthetic 전 임시) | warning |

- **지표 부재를 정상으로 치환하지 않는다(계약 6).** worker 지표가 사라지면 처리율을 0으로 보고 경보가 울린다. promtool 시험으로 확인한다.
- **검사**
  - `make lint-alerts`가 `promtool check rules`와 `test rules`(울려야 할 때와 울리면 안 될 때)를 실행한다. CI의 `compose config` job에서도 실행한다.
  - promtool 이미지는 `PROMETHEUS_IMAGE`로 digest를 고정했다(ADR 0013에 추가).
- **SLO burn-rate 경보**(multiwindow multi-burn-rate)는 SLO 원천과 오류 예산이 정해지는 F11(G1)에서 추가한다. 지금은 D04 §10의 초기 임계 경보만 둔다.

### 4. runbook

- `docs/runbooks/RB01-kafka-backlog-and-store-failure.md`에 경보마다 절(anchor)을 하나씩 둔다. 각 절에는 탐지, 영향, 즉시 조치, 복구 확인을 담는다. 경보의 `runbook_url`이 그 절을 가리킨다.
- **단위 테스트로 고정하는 것**
  - 규칙이 참조하는 지표 이름이 실제 등록된 이름인지 확인한다. 이름이 바뀌면 경보가 조용히 죽는 것을 막는다.
  - 모든 경보에 runbook 절이 있는지 확인한다.
- **D04 §11 금지 사항을 명시했다.** offset을 앞으로 옮기기, queue 삭제, retention 축소로 공간 만들기, `min.insync.replicas` 낮추기는 하지 않는다.

### 5. 로컬

lite 프로필에는 Prometheus를 띄우지 않는다. 메모리 예산 때문이다. 로컬에서는 `curl localhost:9464/metrics`로 확인한다.

### 6. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §1 client_golang + `/metrics` pull | Kubernetes system components ([Metrics for Kubernetes System Components](https://v1-35.docs.kubernetes.io/docs/concepts/cluster-administration/system-metrics/)) | 구성요소가 Prometheus Go client로 계측하고 `/metrics`를 노출하며, 노출 주소를 flag로 분리 | 채택: binary별 registry, 별도 listener |
| §1 별도 실패 영역 | D04 §10 (명세) | 운영 Prometheus를 고객 경로와 분리 | 채택 |
| §2 label·명명 | Prometheus 공식 지침 ([The Zen of Prometheus](https://prometheus.io/docs/practices/the_zen/), Robust Perception [On the naming of things](https://robustperception.io/on-the-naming-of-things)) | 무한 값(user ID 등) label 금지, counter `_total`, 기본 단위(seconds) | 채택: tenant·ID label 금지, `montracer_<component>_<what>_<unit>` |
| §3 경보 단계 | Google SRE Workbook의 multiwindow multi-burn-rate ([Grafana 설명](https://grafana.com/blog/how-to-implement-multi-window-multi-burn-rate-alerts-with-grafana-cloud/)) | SLO 기반 오류 예산 소진율로 page | 보류: SLO 원천(F11) 확정 후 도입. 지금은 D04 §10 초기 임계 |
| §4 경보별 runbook 절 | Grafana Mimir ([Mimir runbooks](https://grafana.com/docs/mimir/latest/manage/mimir-runbooks/)) | mixin 경보마다 같은 이름의 runbook 절(확인 사항, 조치) | 채택: 경보 이름 = 절 anchor, 시험으로 강제 |

- **OpenTelemetry SDK 대신 client_golang을 쓴 이유:** 고객 수집 경로(OTLP)를 쓰지 않는 독립 경로가 요구사항이다(D04 §10). OTel SDK에 Prometheus exporter를 붙여도 pull은 가능하지만, 다음을 이유로 기각했다.
  - 고객 경로와 같은 SDK와 Collector 설정을 공유할 위험(재귀 계측)이 있다.
  - 의존성 크기가 크다.
  - 업계 다수 구성요소(Kubernetes, Mimir, etcd)가 client_golang을 직접 쓴다.
  - platform trace(별도 tenant, 낮은 sampling)는 이 결정과 별개로 OTel SDK를 쓴다(ADR 0001).

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 노출 | 별도 listener pull | 같은 listener: 외부 노출 위험, 고객 경로 과부하 때 지표도 막힘. push(OTLP): 고객 경로와 실패 영역이 겹침 |
| tenant label | 없음 | tenant label: 고객 수만큼 series가 늘어 운영 Prometheus가 cardinality로 무너짐(D04 §10 "bounded top-N 또는 usage 저장소") |
| worker 다운 감지 | 처리율 식에 `or … * 0` | `absent()` 별도 경보: job 이름에 의존하고, 일부 signal만 멈춘 경우를 놓친다 |

## 결과

- G1 차단 항목 중 "수집 metric·RB01"이 해소된다(ADR 0020 §7). quota는 남는다.
- **운영 배포에 필요한 것:** Prometheus scrape 설정(:9464), Kafka exporter와 ClickHouse endpoint scrape, Alertmanager 독립 알림 경로, 외부 heartbeat. 배포 템플릿(helm)에서 만든다.
- **남은 일:** synthetic trace(D04 §10 1분 주기)와 그 실패 경보, SLO burn-rate 경보, RB02~RB04.

## Rollback

지표 listener는 부가 기능이다. 문제가 생기면 binary를 되돌린다. 저장소나 schema 변경은 없다.

## 재검토 조건

- 운영 Prometheus의 series 수가 예산을 넘을 때. 이때는 histogram bucket 수와 label을 재검토한다.
- OTel SDK의 Prometheus 호환 exporter가 의존성·재귀 위험 없이 쓸 수 있는 수준이 될 때.

## 증거

- `internal/opsmetrics` 테스트
  - 값 기록
  - 허용 label 밖 label 거부
  - `/metrics` 응답에 route pattern과 Go runtime 지표 포함
  - 규칙↔지표 이름↔runbook 절 일치. 이름이나 anchor를 바꾸면 실패하는 것을 직접 확인했다.
- `internal/ingest`: Observer가 accepted(append 확인된 것만), append 실패, 401을 구분해 받는다.
- `internal/pipeline`: stage 회계 불변식(소비 = 저장 + 중복 + quarantine), 가장 이른 수신 기준 oldest age, 일시 오류 계수.
- `internal/httpapi`: route pattern만 쓰고 원 URL·ID는 쓰지 않으며, 미등록 경로는 `unmatched`.
- promtool: 규칙 9개 문법 통과. 시험 6개 통과(5xx 10% 울림·0.1% 안 울림, 처리율 절반 울림·worker 지표 없음 울림·따라잡음 안 울림, 신선도 10분 울림).
