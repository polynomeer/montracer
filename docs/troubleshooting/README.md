# 문제 해결 기록 (Problem-Solving Records)

구현·운영 중에 만난 **비자명한 문제**와 그 해결을 기록한다. 커밋 메시지는 "무엇을 고쳤나"를 남기고, 이 기록은 "왜 생겼고, 어떻게 찾았고, 무엇이 재발을 막나"를 남긴다. 같은 함정을 다시 밟지 않고, 설계 가정이 틀렸을 때 ADR을 다시 열 근거로 쓴다.

## 언제 쓰나

다음 중 하나라도 해당하면 **수정 PR에 함께** 쓴다.

- 데이터 유실·중복·오염, tenant 경계, PII, ACK 의미에 닿는 버그(계약 위반 가능성)
- 원인을 찾는 데 30분 이상 걸렸거나, 증상과 원인이 떨어져 있었던 문제
- 테스트·CI·로컬 환경에서 다시 날 수 있는 함정(flaky, 포트 충돌, 자원 부족)
- 리뷰(spec-reviewer 포함)가 찾은 P0·P1 결함
- 작업 절차의 실수(잘못된 커밋 혼입 등)로 생긴 문제

오타, lint 지적, 바로 보이는 단순 버그는 커밋 메시지로 충분하다.

## 작성 규칙

- 파일명은 `PS-NNNN-kebab-case.md`이고, [template.md](template.md)를 복사해 쓴다.
- 고객 데이터, secret, 실제 payload는 넣지 않는다. 재현에는 fixture와 합성 값만 쓴다.
- **재발 방지**에는 시험(파일:함수), 경보, 검사 스크립트처럼 실제로 막는 장치를 적는다. "주의한다"는 재발 방지가 아니다.
- 설계 결정이 바뀌었으면 해당 ADR을 개정하고 서로 링크한다.

## 목록

| ID | 제목 | 영역 | 영향 | 수정 커밋 | 관련 |
|---|---|---|---|---|---|
| [PS-0001](PS-0001-offset-reuse-silent-loss.md) | Kafka offset 재사용 시 ClickHouse insert dedup이 새 데이터를 조용히 버림 | pipeline | 데이터 유실 (P0) | `0b3307a` | ADR 0021 |
| [PS-0002](PS-0002-maxbytes-400-vs-413.md) | 본문 한도 초과가 413이 아니라 400으로 분류됨 | ingress | client의 데이터 폐기 | `13ef63f` | ADR 0017, 0020 |
| [PS-0003](PS-0003-ops-metrics-double-count-and-stall.md) | crash 뒤 운영 지표 이중 계수, 정체를 감지하지 못하는 경보 | 운영 지표 | 경보 오탐·미탐 | `ab8d6da`, `bcb653c` | ADR 0023, RB01 |
| [PS-0004](PS-0004-local-stack-ports-memory.md) | 로컬 stack의 port 충돌·다른 프로젝트 Kafka 오접속·OOM kill | 로컬 환경 | 개발 중단, 오접속 위험 | `23d8364`, `5176e30`, `52825a7` | ADR 0020 |
| [PS-0005](PS-0005-integration-test-shared-state.md) | 공유 topic·tenant별 watermark 때문에 통합 테스트가 불안정함 | 테스트 | flaky CI | `5da123c`, `adbd02a` | ADR 0026 |
| [PS-0006](PS-0006-wip-leaked-into-commit.md) | 다른 작업의 미커밋 변경이 커밋에 섞임 | 작업 절차 | 문서 오기 | `8041941` | — |
| [PS-0007](PS-0007-hidden-text-widens-mobile-page.md) | 표 안의 screen reader용 숨김 글이 375px 화면을 가로로 늘림 | web | 사용성 (P2) | #45 | ADR 0048 |
| [PS-0008](PS-0008-debounced-input-reverted.md) | debounce로 URL에 반영한 뒤 입력 중이던 글자가 되돌려짐 | web | 사용성·flaky 시험 (P2) | #45 | ADR 0043·0045·0047 |
| [PS-0009](PS-0009-service-detail-failure-as-empty.md) | S02 서비스 상세가 조회 실패를 "받은 metric 없음"·빈 표로 보임 | web | 상태 표현(계약 6) | `049c1d2`, `5af9d82` | ADR 0042, 0048 |
| [PS-0010](PS-0010-backfill-empty-under-force-rls.md) | migration backfill이 FORCE RLS 표에서 오류 없이 0행을 옮김(superuser 시험에서는 정상) | controldb | 가용성 — 기존 monitor 미평가 (P1) | E05 B2 PR | ADR 0016, 0051 |

리뷰가 찾아 같은 PR에서 고친 결함(`4ceb222` metric 상충 값 계수, `91a8b69`·`cbeb2e8` 제어 DB 권한 최소화, `c18814f` CI 정규식 오탐)은 해당 ADR과 커밋 메시지에 기록되어 있다.
