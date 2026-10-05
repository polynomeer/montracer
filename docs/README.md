# Montracer 문서

| 종류 | 위치 | 답하는 질문 | 언제 쓰나 |
|---|---|---|---|
| 설계 명세 (권위) | [specs/](specs/README.md) | 무엇을 만들어야 하나 | 원본 docx를 개정하고 재생성한다. 직접 수정하지 않는다 |
| 아키텍처 (구현 기준) | [architecture/](architecture/README.md) | 지금 시스템은 어떻게 생겼나 | 구성 요소, 흐름, 저장소·계정, 신뢰 경계, 실패 동작이 바뀔 때 |
| ADR | [adr/](adr/README.md) | 왜 이렇게 정했나 | 기술 선택, 명세가 정하지 않은 설계 결정, 범위·수치 변경, 명세 간 불일치 해소 |
| 문제 해결 기록 | [troubleshooting/](troubleshooting/README.md) | 무엇이 잘못되었고 무엇이 재발을 막나 | 비자명한 버그, 계약에 닿는 결함, 환경 함정 |
| 실험 | [experiments/](experiments/README.md) | 설계 가정이 실측으로 맞나 | 기술 검증, 부하 시험, 수치 근거가 필요할 때 |
| Runbook | [runbooks/](runbooks/README.md) | 장애가 나면 무엇을 하나 | 경보·운영 영향이 있는 기능을 추가할 때 |
| 계획·상태 | [plan/](plan/work-plan.md) | 어디까지 했나 | 작업 항목·기능 상태가 바뀔 때 |
| 코드 옆 README | `cmd/*/README.md`, `internal/README.md`, `migrations/README.md` 등 | 이 디렉터리는 무엇을 소유하나 | 서비스·패키지를 추가하거나 책임·환경 변수·장애 동작이 바뀔 때 |

## 문서화 규칙

**문서는 코드와 같은 PR에 들어간다.** 문서가 빠진 기능은 완료가 아니다(D06 §02 공통 DoD). 변경 종류별로 반드시 갱신할 문서는 다음과 같다.

| 변경 | 같은 PR에서 갱신할 문서 |
|---|---|
| 명세가 정하지 않은 설계 선택, 라이브러리·저장소·프로토콜 선택, 수치·한도 결정 | 새 ADR (`/adr`) + [adr/README.md](adr/README.md) 등록부 |
| 기존 결정의 변경·보정 | 해당 ADR 개정(변경 이력 절) 또는 대체 ADR |
| 새 서비스·패키지, 구성 요소 간 흐름, 저장소·계정·권한, 실패 동작 | [architecture/README.md](architecture/README.md) + 해당 `cmd/*`·`internal` README |
| API 추가·변경 | ADR(의미·오류·상태 표현) + 서비스 README의 "구현" 줄. OpenAPI가 생기면 `api/openapi` |
| migration | `migrations/README.md`(테이블이 새로 생길 때), 아키텍처 문서의 저장소 절 |
| 경보 추가 | runbook의 경보 절 (anchor) |
| 비자명한 버그 수정·리뷰 P0/P1·환경 함정 | `troubleshooting/PS-NNNN` |
| 수치 근거가 필요한 판단 | `experiments/NNNN` |
| 기능·작업 상태 변화 | [plan/work-plan.md](plan/work-plan.md), [plan/requirements-registry.md](plan/requirements-registry.md) |

작성 원칙

- 결정을 기록할 때는 **고려한 대안과 버린 이유**, 근거(명세 절, 외부 사례 출처, 실측)를 함께 적는다. 결정만 있고 이유가 없으면 다음 사람이 같은 논의를 반복한다.
- 승인자·날짜를 지어내지 않는다. 사용자가 위임한 결정은 `승인 (빅테크 사례 기준 결정 위임)`처럼 근거를 표시한다.
- 문서 간 불일치를 발견하면 임의로 고르지 않는다. 사용자에게 먼저 알리고, 권위 문서 수정 + ADR + 계약 테스트로 해결한다.
- 고객 데이터, secret, 실제 payload는 어느 문서에도 넣지 않는다.
