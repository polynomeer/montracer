## 요약

<!-- 무엇을 왜 바꾸는지 -->

## 추적

- 요구 F ID / Epic: <!-- 예: F01 / E02 -->
- 관련 명세 절: <!-- 예: D02 §04 -->
- ADR: <!-- 해당 시 -->

## 영향

- [ ] Schema 변경 (OpenAPI / proto / migration) — 호환성:
- [ ] Tenant 경계 영향 — 설명:
- [ ] Data retention·삭제 영향 — 설명:
- [ ] PII 수집·처리 변경 — 설명:
- [ ] 해당 없음

## 공통 DoD (D06 §02)

- [ ] 단위 / 통합 / negative test
- [ ] Feature flag off 경로 시험
- [ ] Metric·로그 (secret·payload 미포함)
- [ ] `docs/plan/work-plan.md`·`docs/plan/requirements-registry.md` 상태 갱신

## 문서 ([docs/README.md](../docs/README.md) 문서화 규칙)

- [ ] ADR: 새 설계 선택·수치·라이브러리 결정 (대안·근거 포함) — 번호:
- [ ] 아키텍처 문서·서비스/패키지 README: 구성 요소·흐름·계정·실패 동작 변경
- [ ] 문제 해결 기록 `PS-NNNN`: 비자명한 버그·계약에 닿는 결함·환경 함정
- [ ] 실험 기록: 수치 근거가 필요한 판단
- [ ] 해당 없음 (이유: )
- [ ] Runbook (운영 영향이 있을 때)
- [ ] 새 query·probe·runner 기능이면 abuse case 시험

## Rollout / Rollback

<!-- flag, canary 단계, migration 순서(expand→contract), 되돌릴 수 없는 부분 -->
