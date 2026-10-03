# cmd/worker

Kafka 소비 → 신호별 정규화·dedup → ClickHouse sink, metric window 집계. durable insert 후 offset commit.

- 쓰기 소유 데이터: 신호 원본과 처리 checkpoint
- 동기 의존·장애 동작: sink 실패 시 offset 보류.
- 단계: M0 (F01~F03)
- 명세: D02 §05, §07, §09~10, §21~22
