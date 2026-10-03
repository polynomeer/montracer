# Montracer 설계 명세 (개발 문서 세트 v2.0)

기준일 2026-10-03. 이전 APM 통합 설계서의 범위와 24주 일정은 이 세트로 대체되었다.
설계와 인수 기준이며, 실행 가능한 제품이나 검증 완료 인증서가 아니다.

## 문서 목록과 권위 범위

| 문서 | Markdown | 주요 독자 | 권위(결정) 범위 |
|---|---|---|---|
| D01 제품 요구사항과 벤치마크 | [D01-product-requirements.md](D01-product-requirements.md) | PM, 기술 책임자 | 출시 범위, 기능 ID F01~F27, 목표·비범위, SLO, 벤치마크 출처 B01~B16 |
| D02 시스템 데이터와 API 설계 | [D02-system-data-api.md](D02-system-data-api.md) | Backend, Data | 내구성(ACK), 스키마, 검색, 이벤트, API 계약, 표준 근거 R1~R11 |
| D03 계측과 고급 진단 설계 | [D03-instrumentation-diagnostics.md](D03-instrumentation-diagnostics.md) | Agent, Platform | SDK·Collector, JVM Inspector, thread dump, Profile, DBM, RUM, Synthetic |
| D04 보안 운영과 상용화 | [D04-security-operations-commercial.md](D04-security-operations-commercial.md) | SRE, Security | 격리·RBAC, PII, 보존·삭제, HA/DR, 용량·비용, 과금, Runbook |
| D05 UX UI 디자인 명세 | [D05-ux-ui-design.md](D05-ux-ui-design.md) | Design, Frontend | 정보 구조, 화면 S01~S14, 디자인 토큰, 컴포넌트 상태, 접근성 |
| D06 개발 실행과 품질 계획 | [D06-execution-quality-plan.md](D06-execution-quality-plan.md) | 전 개발팀 | 단계·인력, Epic E01~E10, 테스트, Gate Q0~Q5, CI/CD, ADR 001~012, 레포 규약 |

- 기능 단계는 **D01**, 저장·API 의미는 **D02**, 개인정보 정책은 **D04**가 권위 문서다.
- 문서 간 불일치를 발견하면 구현자가 임의로 고르지 않는다. 권위 문서 수정 + ADR + 계약 테스트 갱신을 함께 한다.
- 참조 표기: `D02 §05` = D02 문서의 05절. `[B05]`는 D01 §09, `[R4]`는 D02 §23의 출처.

## 읽는 순서

1. 모두: D01 → D06 §01~02 (단계·Epic) → [작업계획서](../plan/work-plan.md)
2. Backend/Data: D02 전체 → D04 §01~04
3. Agent/Platform: D03 → D02 §04~07
4. SRE/Security: D04 → D02 §03~05, D06 §03~07
5. Design/Frontend: D05 → D02 §12~16, §19

## 파일 구성

```
docs/specs/
├── README.md          # 이 파일
├── D01~D06-*.md       # docx에서 변환한 Markdown 파생본 (검색·리뷰·Claude Code 참조용)
├── assets/Dxx/        # 문서 내 그림 (아키텍처, 개념 화면)
└── original/          # 권위 원본 docx + 먼저읽기.txt
```

Markdown은 `python3 scripts/docs/convert_specs.py`로 재생성한다 (pandoc 필요).
변환본을 직접 고치지 말고 원본 docx를 개정한 뒤 재생성한다.
