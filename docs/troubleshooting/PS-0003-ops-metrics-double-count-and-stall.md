# PS-0003: crash 뒤 운영 지표 이중 계수, 정체를 감지하지 못하는 경보

- 날짜: 2026-10-05
- 영역: 운영 지표 (`internal/opsmetrics`, `deploy/prometheus/rules`)
- 영향: 경보 오탐·미탐. 장애 중 "정상"으로 보일 수 있음 — P1
- 수정: `ab8d6da`, `bcb653c` (spec-reviewer 지적)
- 관련: ADR 0023, RB01

## 증상

리뷰에서 다음 결함이 발견되었다.

1. worker가 batch 처리 시점에 계수해, crash나 commit 실패 뒤 재처리하면 같은 record를 두 번 셌다. 처리량과 회계 지표가 부풀었다.
2. 신선도 경보는 "가장 오래된 미처리 record의 나이"에 기대었다. worker가 완전히 멈추면 이 값이 갱신되지 않아 경보가 울리지 않았다. 반대로 유휴 뒤 남은 값 때문에 오탐도 났다.
3. client 연결 끊김이 Kafka append 실패로 계수되어 장애 경보가 오탐할 수 있었다.
4. 지표 listener가 serve 중에 실패하면 고객 경로까지 멈출 수 있는 구조였다.

## 원인

지표가 "일을 시도했다"를 셌을 뿐, "일이 확정되었다"(commit)를 세지 않았다. 정체 감지는 진행이 일어나야만 갱신되는 값에 의존했다.

## 해결

- offset commit이 성공한 batch만 센다.
- `montracer_worker_last_commit_timestamp_seconds`로 정체를 판단한다. `MontracerPipelineStalled`는 입력이 있는데 5분간 commit이 없거나 worker 지표가 없을 때 울린다.
- 신선도 경보는 commit이 진행 중일 때만 평가한다.
- client 연결 끊김은 outcome `canceled`로 분리한다.
- listener는 먼저 bind해 기동 시에 실패하게 하고, 이후 serve 오류는 로그만 남긴다.
- `MontracerKafkaUnderReplicated`(ISR 저하) 경보를 추가했다.

## 재발 방지

- `deploy/prometheus/rules/montracer.rules.test.yml` — promtool 시나리오 11개(유휴 시 울리지 않음 포함)
- `internal/pipeline/integration_test.go` — crash 시 실행 0건, 재시작 시 실행 수 = 쓴 record 수
- `internal/opsmetrics/opsmetrics_test.go`

## 교훈

내구성 경계가 있는 파이프라인의 지표는 **경계를 넘은 뒤(commit 후)** 센다. 정체 감지는 "진행이 없을 때도 변하는 값"(마지막 진행 시각과 현재 시각의 차이)으로 한다. 경보마다 "멈췄을 때 울리는가"와 "유휴일 때 조용한가"를 둘 다 시험한다.
