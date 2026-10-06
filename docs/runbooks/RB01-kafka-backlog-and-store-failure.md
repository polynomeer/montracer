# RB01 Kafka 적체와 저장 장애

- 원문: D04 §11 RB01, 지표·경보: D04 §10, ADR 0023
- 관련 설계: ADR 0002(ACK 경계), 0020(ingress·envelope), 0021(worker)
- 경보 규칙: [`deploy/prometheus/rules/montracer.rules.yml`](../../deploy/prometheus/rules/montracer.rules.yml)

## 먼저 알아둘 불변식

1. **ingress는 Kafka에 acks=all로 기록된 뒤에만 200을 준다.** Kafka에 쓰지 못하면 503과 Retry-After를 준다. 이때 데이터는 client(SDK·Collector)의 재시도 큐에 남아 있다. 503은 장애 신호이지만 유실은 아니다.
2. **worker는 ClickHouse 저장 뒤에만 offset을 commit한다.** 저장이 안 되면 lag이 쌓일 뿐이다. 재처리 중복은 insert token, record key, query dedup이 흡수한다.
3. **금지 사항**
   - consumer group offset을 앞으로 옮기지 않는다. 옮긴 구간은 그대로 유실된다.
   - topic이나 queue를 삭제하지 않는다.
   - ClickHouse 테이블을 TRUNCATE하지 않는다.
   - 유실 범위를 확정하기 전에 replay하지 않는다.
4. **Kafka 보존 기간은 24시간이다.** lag이 보존 기간을 넘으면 그 구간은 Kafka에서 사라진다. 아래 "보존 초과" 절차를 따른다.

## 공통 확인 (첫 10분)

| 확인 | 방법 |
|---|---|
| 영향 범위 | Prometheus: `sum by (signal) (rate(montracer_ingress_requests_total{status_class="5xx"}[5m]))`, `time() - montracer_worker_last_commit_timestamp_seconds`(마지막 진행 후 경과) |
| 보존 잔여 시간 | 24h − **아직 처리하지 않은** 가장 오래된 record의 나이. `kafka-consumer-groups.sh --describe`로 partition별 committed offset을 얻고, 그 offset record의 timestamp를 본다(`kafka-console-consumer.sh --partition N --offset <committed> --max-messages 1 --property print.timestamp=true --property print.value=false`). `montracer_worker_oldest_record_age_seconds`는 **이미 저장한** batch의 나이라서 이 계산에 쓰지 않는다 |
| Kafka ISR·lag | `kafka-topics.sh --bootstrap-server $BROKERS --describe --under-replicated-partitions`<br>`kafka-consumer-groups.sh --bootstrap-server $BROKERS --describe --group montracer-worker-raw-v1` |
| worker 상태 | 재시작 횟수(`kube_pod_container_status_restarts_total`), 로그 `sink write failed`·`worker stopped` |
| ClickHouse | 아래 SQL (관리자 계정) |

```sql
-- 저장 지연·실패
SELECT event_time, query_duration_ms, exception_code
FROM system.query_log
WHERE query_kind = 'Insert' AND event_time > now() - INTERVAL 15 MINUTE AND type != 'QueryStart'
ORDER BY event_time DESC LIMIT 20;

-- part 폭증(merge가 insert를 못 따라감)
SELECT table, count() AS parts FROM system.parts WHERE active AND database = currentDatabase() GROUP BY table;
SELECT table, elapsed, progress FROM system.merges;

-- disk
SELECT name, free_space, total_space FROM system.disks;
```

**우선순위:** query, export, backfill 같은 비필수 부하를 먼저 줄인다. 그다음 broker와 replica, 그다음 worker 순서로 복구한다.

---

### MontracerIngressErrorRateHigh

- **탐지:** ingress의 5xx 비율이 0.5%를 넘는 상태가 5분 지속됐다.
- **영향:** 5xx를 받은 client는 재시도한다. 계속되면 client 큐가 넘쳐 SDK 쪽에서 drop이 생긴다(`otelcol_exporter_send_failed_*`, SDK `dropped`).
- **즉시 조치**
  1. 같은 시각에 `MontracerIngressKafkaAppendFailing`이 있는지 본다. 있으면 그 절을 따른다.
  2. 없으면 인증 저장소(PostgreSQL) 장애를 본다. 인증 저장소에 닿지 않으면 503 fail closed가 정상 동작이다. 제어 DB의 연결과 HA 상태를 확인한다.
  3. 둘 다 아니면 ingress 로그에서 `ingest decode`·`ingest prepare` 오류(500)를 찾는다. 이것은 코드 결함이다. 직전 배포를 되돌린다.
- **복구 확인:** 5xx 비율이 0.5% 미만으로 15분 유지되고, `montracer_ingress_records_total{outcome="accepted"}` 증가율이 평시 수준으로 돌아온다.

### MontracerIngressKafkaAppendFailing

- **탐지:** acks=all append 실패가 5분 지속됐다(`montracer_ingress_kafka_append_duration_seconds_count{outcome="error"}`).
- **흔한 원인:** ISR이 `min.insync.replicas`(2) 미만(`NOT_ENOUGH_REPLICAS`), broker disk 가득 참, 네트워크 분리.
- **즉시 조치**
  1. under-replicated partition과 broker disk를 확인한다. disk가 80%를 넘으면 broker나 storage를 증설한다. **retention을 줄여 공간을 만드는 것은 미처리 데이터 유실이므로 하지 않는다.**
  2. 죽은 broker를 복구하거나 교체한다. `min.insync.replicas`를 낮추는 것은 내구성 계약(ADR 0002) 위반이라 금지한다.
  3. 용량이 부족하면 tenant quota의 비필수 입력 제한을 적용한다(D04 §11). quota 구현 전에는 Collector 쪽 sampling을 고객과 협의한다.
- **복구 확인:** append 실패가 0이고, ingress 5xx가 정상이고, worker 처리율이 승인률을 따라잡는다(`MontracerWorkerFallingBehind` 해소).

### MontracerWorkerFallingBehind

- **탐지:** worker 처리율이 ingress 승인률의 99% 미만인 상태가 10분 지속됐다. worker 지표가 아예 없어도 울린다(전부 다운).
- **즉시 조치**
  1. worker pod가 떠 있는지, 재시작을 반복하는지 확인한다. 재시작 반복이면 `MontracerWorkerStoreFailing` 절을 따른다.
  2. 떠 있는데 느리면 ClickHouse 저장 시간(`montracer_worker_store_duration_seconds`)과 part 수를 본다. part가 폭증했으면 insert 빈도가 merge를 넘은 것이다. worker를 늘리기 전에 merge 상태를 먼저 해결한다.
  3. 저장소가 정상인데도 느리면 worker를 늘린다. partition 수(기본 6)가 상한이다.
- **복구 확인:** 처리율이 승인률 이상이고, `montracer_worker_oldest_record_age_seconds`가 감소해 1분 이하가 된다.

### MontracerPipelineStalled

- **탐지:** 입력은 들어오는데 worker의 offset commit이 5분 넘게 없다. worker 지표가 아예 없어도 울린다(전부 다운, 재시작 반복).
- **영향:** 이 동안 저장되는 데이터가 없다. Kafka 보존(24h)이 줄어들고 조회 결과가 멈춘다.
- **즉시 조치**
  1. worker pod 상태와 재시작 횟수를 본다. 로그의 마지막 `worker stopped` 원인을 확인한다.
  2. `sink failed ... after N attempts`이면 `MontracerWorkerStoreFailing` 절을 따른다.
  3. `commit offsets`이면 `MontracerWorkerCommitFailing` 절을 따른다.
  4. 지표 listener 기동 실패(`metrics listener`)면 port 충돌을 확인한다.
  5. 보존 잔여 시간(위 공통 확인)을 계산해 공유한다.
- **복구 확인:** `time() - montracer_worker_last_commit_timestamp_seconds`가 계속 작게 유지된다. `MontracerWorkerFallingBehind`와 `MontracerPipelineFreshnessLag`도 해소된다.

### MontracerPipelineFreshnessLag

- **탐지:** commit은 진행 중인데, 최근 commit한 batch의 record가 수신 후 5분 넘게 지나 저장됐다. 적체를 처리하고 있다는 뜻이다(D04 §11 RB01 탐지 조건 "oldest lag > 5분").
- **영향:** 조회 결과가 늦다. alert 평가는 watermark 이전 window를 쓰므로, 이 상태는 NO_DATA나 늦은 경보로 이어질 수 있다(D02 §21).
- **즉시 조치:** `MontracerWorkerFallingBehind` 절과 같다. 보존 잔여 시간을 계산해 incident 채널에 공유한다. 잔여가 6시간 미만이면 SEV1로 올린다(대량 유실 위험).
- **복구 확인:** lag 1분 이하, stage 회계 일치(아래).

### MontracerKafkaUnderReplicated

- **탐지:** 수집 topic에 under-replicated partition이 5분 넘게 있다(kafka_exporter 지표). ISR이 `min.insync.replicas`(2) 아래로 내려가면 acks=all append가 실패하고 ingress가 503을 준다.
- **즉시 조치**
  1. `kafka-topics.sh --describe --under-replicated-partitions`로 빠진 broker를 찾는다.
  2. broker process, disk, network를 복구한다. 복구 불가면 broker를 교체하고 partition 재할당을 한다.
  3. 재할당은 throttle을 걸어 live 처리량을 지킨다. `min.insync.replicas`를 낮춰 우회하지 않는다(ADR 0002).
- **복구 확인:** under-replicated 0, `MontracerIngressKafkaAppendFailing` 해소.

### MontracerWorkerStoreFailing

- **탐지:** ClickHouse 저장의 일시 실패가 5분 반복됐다. worker는 같은 token으로 60초 재시도한 뒤 commit 없이 종료되고 재시작된다(ADR 0021 §7). 이 동작은 정상이고 데이터는 Kafka에 남아 있다.
- **즉시 조치**
  1. worker 로그의 `clickhouse exception code N`을 본다.

     | 코드 | 의미 | 조치 |
     |---|---|---|
     | 241 | MEMORY_LIMIT_EXCEEDED | ClickHouse 메모리와 동시 query를 확인한다. 비필수 query를 줄인다 |
     | 242 | TABLE_IS_READ_ONLY | replica·Keeper 장애다. replica를 복구한다 |
     | 252 | TOO_MANY_PARTS | merge 적체다. insert 빈도와 merge 상태를 확인한다 |
     | 209·210 | 네트워크 | 연결 경로를 확인한다 |
     | 497 | 권한 | ingest role의 GRANT가 빠졌다. migration 상태를 확인한다 |

  2. disk가 80%를 넘으면 storage를 증설한다. TTL이 아직 지우지 않은 만료 데이터는 query가 이미 숨기고 있다. 그러니 그 데이터를 지우려고 mutation을 서두르지 않는다.
- **복구 확인:** `montracer_worker_sink_errors_total{kind="transient"}` 증가가 멈추고, `montracer_worker_offset_commits_total{outcome="ok"}`가 증가한다.
- `kind="unrecoverable"`이 보이면 quarantine 행 자체를 저장소가 거부한 것이다. 이것은 코드 결함이다. 그 partition은 멈춘다. 직전 배포를 되돌린다.

### MontracerWorkerRowsRejected

- **탐지:** 드라이버가 특정 행을 결정적으로 거부해, 그 행을 `sink_rejected` quarantine으로 돌렸다(ADR 0021 §7). partition은 계속 진행 중이다.
- **의미:** 정규화 코드와 schema가 어긋났다는 결함 신호다. 장애는 아니지만 그 record는 저장되지 않았다.
- **조치**
  1. 대상을 찾는다. 원문은 없고 위치와 해시만 있다.
     ```sql
     SELECT topic, kafka_partition, kafka_offset, event_id, tenant_id, quarantined_at
     FROM ingest_quarantine WHERE reason = 'sink_rejected' AND quarantined_at > now() - INTERVAL 1 DAY;
     ```
  2. 24시간 안에 코드를 고치고 배포한다. 해당 offset은 Kafka 보존 안에 있으니 replay job으로 다시 처리한다(D02 §05 replay 절차: job_id, 대상 offset, 정책 버전, 예상량 기록, live 여유의 20%까지만).
  3. 24시간을 넘기면 그 record는 유실이다. 유실 범위(tenant, signal, 건수, 시간)를 기록한다.

### MontracerWorkerQuarantineRatioHigh

- **탐지:** 소비한 record 중 1% 넘게 quarantine됐다(10분).
- **조치:** 먼저 사유별로 나눠 본다.
  ```sql
  SELECT reason, signal, count() FROM ingest_quarantine
  WHERE quarantined_at > now() - INTERVAL 1 HOUR GROUP BY reason, signal ORDER BY count() DESC;
  ```

  | 사유 | 의미 | 조치 |
  |---|---|---|
  | `unknown_schema_version` | ingress가 worker보다 새 envelope 형식을 쓴다 | 배포 순서 오류다. worker를 먼저 올리거나 ingress를 되돌린다 |
  | `tenant_key_mismatch`, `missing_header`, `invalid_header` | raw topic에 ingress가 아닌 producer가 썼다 | **보안 사건으로 다룬다.** Kafka ACL을 확인하고 RB03을 함께 연다 |
  | `conflicting_point_value` | 같은 stream·시각에 다른 값이 왔다 | 고객 계측 충돌(두 writer)이거나 redaction이 series를 합친 경우다(ADR 0019 §5). tenant별로 모아 고객에게 알린다 |
  | `event_id_mismatch`, `decode_failed`, `not_single_record` | ingress envelope 생성 결함 | 직전 ingress 배포를 되돌린다 |

### MontracerWorkerCommitFailing

- **탐지:** 저장은 됐지만 offset commit이 실패했다. 다음 소유자가 같은 범위를 다시 쓰고, token·record key가 중복을 흡수한다.
- **조치:** consumer group coordinator와 broker 상태를 확인한다. 반복되면 `__consumer_offsets` topic의 ISR을 확인한다.

### MontracerMetricRollupStalled

- **탐지:** metric 1분 rollup(`MONTRACER_WORKER_ROLES=rollup` process)이 5분 넘게 성공하지 못했다.
- **영향:** 원본 `metric_points`는 계속 저장된다. 그러나 `metric_1m`을 쓰는 dashboard·monitor 값이 멈춘다. 10분이 지나도록 계산하지 못한 window는 재계산 범위를 벗어난다. 그 구간은 backfill job으로만 채울 수 있다(D02 §07).
- **즉시 조치**
  1. rollup process가 떠 있는지 본다. **cluster에 하나만** 떠 있어야 한다(ADR 0026 §4). 로그 `metric rollup cycle failed`의 원인을 확인한다.
  2. ClickHouse가 원인이면 `MontracerWorkerStoreFailing`의 코드표를 따른다. 497이면 rollup role의 GRANT(migration 00003)를 확인한다.
  3. 실행 시간(`montracer_rollup_cycle_duration_seconds`)이 주기(30초)를 넘으면 원본 양에 비해 계산 범위가 크다는 뜻이다. 부하 시험 결과를 보고 ADR 0026 재검토 조건을 따른다.
- **복구 확인:** `montracer_rollup_cycles_total{outcome="ok"}`가 증가하고, 경보가 해소된다.
- **10분 넘게 멈췄다면:** live rollup은 그 구간을 다시 계산하지 않는다. 로그 `metric rollup gap beyond catch-up limit; backfill required`의 `tenant_id`·`from`·`resume`으로 구간을 확인하고 아래 "metric backfill"을 돌린다.

### MontracerQuotaOverridesInvalid

- **탐지:** `MONTRACER_QUOTA_OVERRIDES_FILE`을 다시 읽다 실패했다. JSON 오류, 알 수 없는 필드, 대문자 tenant UUID, 일부 값만 지정한 한도 등이 원인이다.
- **영향:** 직전에 성공한 overrides가 계속 적용된다. 장애는 아니지만, 의도한 한도 변경이 반영되지 않았다.
- **조치:** ingress 로그 `quota overrides reload failed`의 원인을 보고 파일을 고친다. 아래 "tenant quota 조정"의 형식을 따른다.

### MontracerSyntheticProbeFailing

- **탐지:** platform-probe(ADR 0031)의 검사 하나가 2회 연속 실패했다. probe는 1분마다 probe tenant로 3-span trace, 연결 log, exemplar 달린 gauge를 공개 ingress에 보내고 조회 API로 확인한다.
- **check별 의미와 첫 조치**

  | check | 의미 | 먼저 볼 것 |
  |---|---|---|
  | `ingest_traces`·`ingest_logs`·`ingest_metrics` | ingress가 그 signal에 200을 주지 않았거나 일부를 거절했다 | `MontracerIngressErrorRateHigh`·`KafkaAppendFailing` 동시 발생 여부. 429·413이면 probe tenant quota, 401·403이면 probe key 만료·폐기. metric만 거절이면 series 등록부(503)·cardinality 상한(ADR 0029·0030) |
  | `trace` | 60초 안에 3 span·complete로 조회되지 않았다(경로 단절·지연) | 위 "공통 확인"의 lag·freshness, `MontracerWorkerStoreFailing`, query-api 5xx. 사유 `partial trace`면 일부 span만 저장된 것이다 |
  | `redaction`·`isolation` | 보안 check는 이 경보가 아니라 `MontracerSyntheticRedactionFailing`·`MontracerSyntheticIsolationFailing`으로 울린다 | [RB03](RB03-pii-exposure-and-access-breach.md) |

- **이 경보는 수집·조회 경로 check(`ingest_*`·`trace`)만 본다.** 경로가 끊겨 redaction·isolation을 평가하지 못하면 그 check는 `outcome="blocked"`로 세고 보안 경보도 울리지 않는다.
- **로그:** probe 로그 `synthetic probe check failed`의 `reason`(고정 문구)을 본다. 응답 본문은 남기지 않는다.
- **복구 확인:** 해당 check의 `montracer_probe_consecutive_successes{check}` ≥ 3(D04 §11 "3회 연속 성공").

### MontracerSyntheticProbeNotRunning

- **탐지:** probe가 약 5분째 결과를 내지 않았거나(첫 주기 전에 죽는 crash loop 포함) 지표 자체가 없다. 그동안 위 경보는 울리지 않는다(감시 공백).
- **배포 전:** probe를 배포하기 전에 규칙을 적용하면 이 ticket이 열린다. rollout 순서는 ADR 0031 "Rollout"이다.
- **조치:** platform-probe process와 운영 Prometheus의 scrape 대상을 확인한다. 기동 실패면 로그의 설정 오류(`MONTRACER_PROBE_*`)를 본다. probe key는 secret manager에서 주입한다.

### MontracerServiceCatalogStale

- **탐지:** ingress의 서비스 catalog 등록(ADR 0038)이 15분째 실패하거나(`write_error`) queue가 넘쳐 버려진다(`dropped`).
- **영향:** 수집·조회는 정상이다. 다만 새 서비스가 `GET /api/v1/services`에 나타나지 않는다. 기존 서비스의 `last_seen`도 멈춰 24시간 뒤 `inactive`로 잘못 보인다.
- **조치**
  - `write_error`: 제어 DB(PostgreSQL) 상태와 ingress 로그 `service catalog write failed`를 본다. migration 00006(`services`)이 적용됐는지 확인한다.
  - `dropped`: 등록이 수신을 못 따라간다. 제어 DB 지연을 먼저 본다. 같은 서비스는 5분마다만 쓰므로, 서비스 수가 급증했는지도 본다.
- **복구 확인:** `montracer_ingress_catalog_services_total{outcome="written"}`가 늘고 경보가 해소된다. 다음 요청부터 자동으로 다시 등록된다.
- **한계:** 장애 동안 버려진 서비스 중 그 뒤 다시 보내지 않는 것(1회성 batch job 등)은 catalog에 빠진 채로 남는다. 원본 backfill은 아직 없다(ADR 0038 §4). 고객이 "원본에는 있는데 서비스 목록에 없다"고 하면 이 경우다. 그 서비스가 다시 보내면 등록된다.
- **`over_limit`(경보 대상 아님):** tenant가 서비스 상한(5,000)에 닿았다. 대개 `service.name`에 pod ID·버전 같은 가변 값이 섞인 계측 오류다. 고객에게 이름 규칙을 고치게 안내한다. 상한은 근사이고(replica 수 × batch만큼 넘을 수 있다), 이미 있는 서비스의 갱신은 계속된다.

## metric backfill (ADR 0035)

live rollup이 다시 계산하지 않는 구간을 원본에서 채운다. 원본에는 저장됐는데 metric 조회에서 `no_data`이거나 값이 모자란 경우다.

- **언제**
  - rollup 정체 뒤의 gap
  - 10분 넘게 늦게 온 point(수집 허용은 과거 24시간)
  - 처음 보는 tenant의 과거 구간
- **실행:** rollup 계정이 있는 곳(worker와 같은 환경)에서 돌린다.
  ```bash
  MONTRACER_CH_ROLLUP_DSN=... worker backfill --tenant <tenant UUID> --from 2026-10-06T09:00:00Z --to 2026-10-06T11:00:00Z
  ```
  - `--resolution`은 기본 `all`(1m·1h)이다. 1h는 live 1h 재계산 구간 앞까지만 계산한다.
  - `--to`는 live 재계산 구간 앞이어야 한다(1m은 약 12분 전, 1h는 직전 닫힌 시간의 1시간 전). 그 뒤는 live rollup이 맡는다.
  - `--from`은 원본 보존(15일)보다 1시간 남짓 여유가 있어야 한다.
  - tenant 하나씩 돌린다. 로그의 `job_id`·`windows`를 incident에 기록한다.
  - 실패하면 오류의 `done up to` 시각부터 다시 돌린다.
- **삭제:** 삭제 대상(삭제 job이 생기면)인 tenant·범위에는 돌리지 않는다(ADR 0035 §4).
- **부하:** chunk(1m은 1시간 분량) 사이에 쉬며 돈다. chunk 하나가 원본 200만 point를 넘으면 멈추므로 범위를 나눈다. 큰 범위는 업무 시간 밖에 돌리고, `montracer_rollup_cycle_duration_seconds`(live)가 늘지 않는지 본다.
- **멱등:** 다시 돌려도 같은 값이다.
- **경보:** backfill로 바뀐 값 때문에 이미 발송된 경보가 취소되지는 않는다(D02 §07).
- **원본 보존(15일)보다 오래된 구간은 다시 만들 수 없다.** 유실 범위로 기록한다.

## tenant quota 조정 (비필수 입력 제한, D04 §11)

용량이 부족하거나 한 tenant가 cluster를 압박하면 그 tenant의 한도를 낮춘다. 반대로 계약 상향이면 올린다(ADR 0024).

1. overrides 파일(`MONTRACER_QUOTA_OVERRIDES_FILE`)에 tenant·signal 항목을 넣는다. **네 값을 모두 지정한다.** 값은 cluster 전체 기준이고, 각 ingress가 replica 수로 나눈다.
   ```json
   {"tenants": {"<tenant UUID 소문자>": {"logs": {"records_per_second": 2000, "records_burst": 40000,
                                                 "bytes_per_second": 3000000, "bytes_burst": 10000000}}}}
   ```
2. 10초 안에 반영된다(`montracer_ingress_quota_overrides_reloads_total{outcome="ok"}` 증가, 로그 `quota overrides reloaded`).
3. **확인:** 해당 tenant 요청이 429(rate) 또는 413(burst 초과)을 받는다. ingress 로그의 `quota_limit` 필드와 `montracer_ingress_records_total{outcome="rejected",reason="rate_limited"}`로 본다.
4. **주의 사항**
   - **tenant를 줄일 때는 rate를 낮춘다.** burst를 정상 최대 batch보다 작게 하면 그 batch가 413을 받고 client가 버린다. byte burst는 해제 본문 상한(8MiB) 아래로 내려가지 않는다(코드가 하한을 적용한다). burst는 replica 수로 나누지 않는다.
   - 429 로그의 `quota_rate_per_replica`·`quota_replicas`·`quota_overridden`으로 어떤 한도가 걸렸는지 확인한다.
   - **429는 계약 초과라서 수집 가용성 SLO 분모에서 빠진다(D01 §08).** 따라서 용량 부족을 quota 하향으로 감추지 않는다. 용량 부족은 503(과부하)과 증설로 다룬다.
   - **`MONTRACER_INGRESS_REPLICAS`는 실제 replica 수와 맞춘다.** 실제보다 크게 잡으면 tenant 한도가 그만큼 줄고, 작게 잡으면 한도를 넘는다.

---

## 보존 초과 (lag > 24시간에 근접)

1. 남은 시간 안에 처리를 따라잡을 수 있는지 계산한다(처리율 대비 적체량).
2. 따라잡을 수 없으면 incident commander가 결정한다. **offset을 옮기지 않는다.** 보존 기간이 지나 사라지는 구간이 유실 범위다.
3. 유실 범위를 기록한다: topic·partition별 offset 범위, 시각, tenant·signal별 건수 추정. 이 수치는 고객 공지와 과금 보정(D04 §09)의 입력이 된다.

## 복구 확인 (D04 §11 RB01)

- lag 1분 이하: commit이 진행 중이고(`time() - montracer_worker_last_commit_timestamp_seconds` < 60), `montracer_worker_oldest_record_age_seconds` ≤ 60, consumer group lag(records)이 평시 수준.
- synthetic 3회 연속 성공: `montracer_probe_consecutive_successes{check="trace"}` ≥ 3, 그리고 ingest_*·redaction·isolation(배포된 경우)도 ≥ 3 (ADR 0031). probe가 배포되지 않은 환경에서는 known trace를 수동으로 보내 `GET /api/v1/traces/{id}`로 조회한다.
- stage 회계 일치(같은 window): `montracer_ingress_records_total{outcome="accepted"}` 증가량 ≈ `montracer_worker_records_total` 증가량.
  - worker 쪽은 stored + duplicate + quarantined를 모두 더한 값이다.
  - in-flight 때문에 window 경계에서는 작은 차이가 정상이다. window를 닫은 뒤 비교한다(D02 §21).

## 사후 기록

- 타임라인: 탐지, 첫 조치, 완화, 복구 시각.
- 영향: tenant·signal별 지연 시간, 503 건수(client 재시도로 회복됐는지), 유실 범위(있으면).
- 원인과 재발 방지: 경보 임계, 용량, 코드 결함.
- 이 runbook에서 틀렸거나 빠진 절차를 같은 PR에서 고친다.
