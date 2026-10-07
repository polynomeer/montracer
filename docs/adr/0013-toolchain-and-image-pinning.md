# ADR 0013: 런타임·인프라 버전 고정

- 상태: 승인
- Owner: Platform
- 승인자: polynomeer
- 날짜: 2026-10-03 (결정)
- 관련: 작업계획서 §5.0, §8 #3 · D02 §01 "버전과 라이선스 관리" · D06 §07, §10

## 배경

D02 §01은 "구현 시작 시 지원 중인 안정 버전을 선정하고 이미지 digest와 lockfile로 고정, main·latest 금지"를 요구한다. 로컬 stack·CI·Go module·pnpm workspace를 만들려면 버전이 먼저 정해져야 한다.

## 결정

2026-10-03 기준 지원 중인 안정 버전으로 고정한다. 이미지는 multi-arch index digest로 참조한다.

| 구성 요소 | 버전 | 고정 위치 |
|---|---|---|
| Go | 1.26 (toolchain go1.26.8) | `go.mod` (`go`, `toolchain`), CI |
| Go module path | `github.com/polynomeer/montracer` | `go.mod` |
| Node.js | 24 LTS (24.21.0) | `.nvmrc`, `package.json` `engines`, CI |
| pnpm | 11.28.2 | `package.json` `packageManager` (corepack), `pnpm-lock.yaml` |
| PostgreSQL | 17.11 | `deploy/compose`, image digest |
| Apache Kafka (KRaft) | 4.3.1 | `deploy/compose`, image digest |
| ClickHouse | 26.8.15.10 (26.8 LTS) | `deploy/compose`, image digest |
| OTel Collector contrib | 0.161.0 | `deploy/compose`, image digest |
| Prometheus (promtool) | 3.13.2 | `deploy/compose/versions.env` image digest — 경보 규칙 검사 (2026-10-05 추가, ADR 0023) |
| TypeScript | 7.0.2 | `apps/web`·`packages/design-tokens` devDependencies, `pnpm-lock.yaml` (2026-10-07 추가, ADR 0041) |
| React · React DOM | 19.3.0 | `apps/web` dependencies (2026-10-07 추가, ADR 0041) |
| React Router | 8.4.0 | `apps/web` dependencies (2026-10-07 추가, ADR 0041) |
| Vite · @vitejs/plugin-react | 8.3.3 · 6.1.2 | `apps/web` devDependencies (2026-10-07 추가, ADR 0041) |
| Vitest · jsdom · Testing Library(react · user-event) | 5.0.3 · 30.1.2 · 16.3.3 · 14.6.7 | `apps/web` devDependencies (2026-10-07 추가, ADR 0041) |
| @types/react · @types/react-dom · @types/node | 19.3.0 · 19.3.0 · 24.19.1 | devDependencies. `@types/node`는 런타임 Node 24에 맞춘다 (2026-10-07 추가, ADR 0041) |
| Pretendard(글꼴) | 1.3.9 | `apps/web` dependencies, SIL OFL 1.1 (2026-10-07 추가, ADR 0041) |

이미지 digest의 단일 원천은 `deploy/compose/versions.env`다.

선택 이유
- Go: 1.27이 최신이지만 1.26도 지원 중인 두 minor 중 하나이고 로컬 개발 환경이 1.26이다. P0 종료 시 1.27 전환을 검토한다.
- PostgreSQL: 18이 최신이지만 설계 근거(R9)가 17 문서 기준이고, 17은 장기 지원 중이다.
- ClickHouse: LTS 라인(26.8)을 사용한다. 최신 stable(26.9)은 쓰지 않는다.
- OTel Collector: 0.162.0은 multi-arch index가 아직 없어 0.161.0을 쓴다.
- Prometheus: 경보 규칙 문법·단위 시험(promtool)에 최신 stable 3.13.2를 쓴다. 운영 Prometheus 버전은 배포 템플릿에서 같은 값을 쓴다.
- pnpm: 최신 major는 12지만, 팀 로컬 환경(11)과 맞추고 11 라인의 최신 patch를 쓴다.

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. 지원 중인 안정/LTS 버전 + digest 고정 (채택) | 재현성, 공급망 통제 | 주기적 갱신 작업 필요 |
| B. 항상 최신 major | 신기능 | 초기 버그·생태계 미성숙 |
| C. floating tag (`17`, `latest`) | 갱신 수고 없음 | 비재현 빌드, D02 §01 위반 |

## 결과

- 버전 갱신은 이 ADR의 표와 `deploy/compose/versions.env`, `go.mod`, `package.json`을 같은 PR에서 바꾼다.
- Dependabot/Renovate 같은 자동 갱신 도구를 쓰더라도 digest 변경 PR은 CI 통과 후 사람이 merge한다.

## Rollback

이전 digest로 되돌린다. 데이터 포맷이 바뀌는 major 업그레이드(PostgreSQL, ClickHouse, Kafka)는 별도 ADR과 migration 계획이 필요하다.

## 재검토 조건

- 분기마다, 또는 고정 버전이 지원 종료·보안 취약점 공지를 받을 때.
- P0 종료 시점 (Go 1.27, PostgreSQL 18 검토).

## 증거

- 2026-10-03 Docker Hub·nodejs.org·go.dev·npm registry 조회 결과.
