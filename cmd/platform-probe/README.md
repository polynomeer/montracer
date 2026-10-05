# cmd/platform-probe

플랫폼 synthetic probe. probe tenant로 고객과 같은 공개 경로(ingress → Kafka → worker → ClickHouse → query-api)를 1분마다 지나가며 조회·연결·정책 적용을 검증한다.

- 쓰기 소유 데이터: 없음(probe tenant의 telemetry만 공개 경로로 보낸다)
- 동기 의존·장애 동작: ingress·query-api의 공개 endpoint. 실패는 지표로만 내보내고 재시도하지 않는다(다음 주기가 재시도). probe 자체 정지는 `MontracerSyntheticProbeNotRunning`.
- 단계: G1 (D04 §10 플랫폼 자체 관측)
- 명세: D04 §10, §11 · ADR 0031
- 구현: `cmd/platform-probe`(조립) → `internal/probe`(검사) · 지표 `internal/opsmetrics.NewProbe`
- 실행 환경 변수: `MONTRACER_PROBE_INGRESS_URL`, `MONTRACER_PROBE_QUERY_URL`, `MONTRACER_PROBE_INGEST_KEY`, `MONTRACER_PROBE_API_KEY`, `MONTRACER_PROBE_OTHER_API_KEY`(선택, 격리 검사), `MONTRACER_PROBE_ENVIRONMENT`(기본 synthetic), `MONTRACER_PROBE_INTERVAL`(기본 1m), `MONTRACER_METRICS_ADDR`(기본 :9464)
- key는 secret manager에서 주입한다. probe tenant·key는 운영자가 미리 만든다(자동 준비는 control-api 이후).
- 운영 지표: `montracer_probe_*`, 경보 `MontracerSyntheticProbeFailing`·`MontracerSyntheticProbeNotRunning` (RB01)
- 아직 없는 것: metric·log 조회 검증, 5분 synthetic monitor 알림 callback, 다중 지역 (ADR 0031 §4)
