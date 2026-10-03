# cmd/profile-worker

profile·replay 객체 업로드 정제: pending manifest → upload gateway → checksum·size·redaction 검증 → 정제 객체 저장 → ready manifest commit.

- 쓰기 소유 데이터: 정제 객체, index manifest
- 동기 의존·장애 동작: 객체 finalize 전 검색 비노출. 정제 실패 원문 미저장, orphan GC.
- 단계: G2 (F15), G3 (F21)
- 명세: D02 §03 · D03 §07, §10
