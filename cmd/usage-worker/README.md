# cmd/usage-worker

불변 usage ledger와 정정 항목 기록, 청구 단위 대사.

- 쓰기 소유 데이터: usage ledger
- 동기 의존·장애 동작: 대사 실패 시 청구 보류(invoice hold).
- 단계: G1 (F09)
- 명세: D04 §13
