# cmd/alert-worker

monitor 평가(lease + idempotency), 상태 머신(OK/PENDING/ALERT/RECOVERING/NO_DATA/EVALUATION_ERROR), notification outbox·webhook 전송.

- 쓰기 소유 데이터: evaluation·episode·notification
- 동기 의존·장애 동작: query 실패는 EVALUATION_ERROR (이전 값으로 대체 금지).
- 단계: M0 (F06), G1 (F11)
- 명세: D02 §17, §21
