# PS-0005: 공유 topic·tenant별 watermark 때문에 통합 테스트가 불안정함

- 날짜: 2026-10-05
- 영역: 테스트 (`internal/pipeline`, `internal/rollup` 통합 시험)
- 영향: flaky CI — P2
- 수정: `5da123c`, `adbd02a`
- 관련: ADR 0021, 0026

## 증상

1. pipeline 통합 테스트의 회계 등식(소비 = 저장 + 중복 + quarantine)이 가끔 깨졌다.
2. rollup의 watermark를 tenant별로 바꾼 뒤 rollup 통합 테스트가 기대한 window를 만들지 못했다.

## 원인

1. Go는 package별 테스트를 병렬로 돌린다. 다른 package(ingest)의 통합 테스트가 같은 Kafka topic에 동시에 써서, pipeline 테스트의 worker가 그 record까지 소비했다.
2. watermark가 tenant별로 계산되므로, 최신 point가 한 tenant에만 있으면 다른 tenant의 window는 닫히지 않는다.

## 해결

1. 통합 테스트의 회계 검사를 하한(≥)으로 바꿨다. 정확한 등식은 외부 간섭이 없는 단위 테스트가 고정한다.
2. 최신 point를 tenant마다 둔다.

## 재발 방지

- 공유 자원(topic, consumer group, 테이블)을 쓰는 통합 테스트는 tenant를 무작위로 만들고, **다른 테스트의 데이터가 섞여도 참인 단언**만 둔다.

## 교훈

설계를 바꿀 때(전역 watermark를 tenant별로) 시험 fixture의 전제도 같이 바뀐다. ADR의 "시험" 절에 fixture 전제를 적어 두면 같이 고칠 곳이 보인다.
