# cmd/control-api

tenant·멤버·키·정책·dashboard·monitor·삭제 job 관리 API. 변경·감사·outbox를 같은 PostgreSQL 트랜잭션에 기록.

- 쓰기 소유 데이터: tenant·정책·revision·outbox (PostgreSQL, RLS)
- 동기 의존·장애 동작: PostgreSQL 불가 시 mutation 중단.
- 단계: M0 (F05~F07)
- 명세: D02 §11, §14, §20 · D04 §01, §12
