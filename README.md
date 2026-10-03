# Montracer

서비스 상태 파악부터 요청 단위 원인 조사, JVM 내부 진단, 사용자 경험 확인까지 연결하는 B2B 관측(APM) 플랫폼.

> 현재 상태: **Phase 0 (레포 부트스트랩).** 모노레포 골격만 있고 실행 가능한 코드는 아직 없다.
>
> 시작하기: `make doctor` → `make bootstrap` → `make help`

## 빠른 안내

| 무엇을 | 어디서 |
|---|---|
| 설계 명세 D01~D06 (색인·읽는 순서) | [docs/specs/README.md](docs/specs/README.md) |
| 전체 작업계획서 | [docs/plan/work-plan.md](docs/plan/work-plan.md) |
| 기능(F01~F27) 상태 | [docs/plan/requirements-registry.md](docs/plan/requirements-registry.md) |
| 아키텍처 결정(ADR) | [docs/adr/](docs/adr/README.md) |
| 운영 Runbook | [docs/runbooks/](docs/runbooks/README.md) |
| Claude Code 작업 지침 | [CLAUDE.md](CLAUDE.md) |

## 저장소 구조

```
montracer/
├── CLAUDE.md                 # Claude Code 프로젝트 지침
├── .claude/                  # Claude Code 공유 설정 (settings, agents, skills)
├── Makefile                  # 개발 경험 계약 (make help)
├── apps/web/                 # React + TypeScript UI
├── cmd/                      # Go 서비스 진입점 (ingress, query-api, control-api, worker, alert-worker, …)
├── internal/                 # 공유 Go 패키지 (authz, telemetry, query, pipeline)
├── api/                      # OpenAPI 3.1, OTLP·내부 event proto
├── migrations/               # postgres, clickhouse
├── deploy/                   # compose(로컬), helm
├── infra/                    # 관리형 서비스 IaC
├── agents/  sdk/  packages/  integrations/   # 확장 모듈 (D06 §11)
├── tests/                    # fixtures, contract, isolation, e2e, load
├── docs/
│   ├── specs/                # 설계 명세 Markdown + assets + original(docx 원본)
│   ├── plan/                 # 작업계획서, 요구사항 registry
│   ├── adr/                  # 결정 기록
│   └── runbooks/             # 운영 절차
└── scripts/                  # dev(doctor), docs(변환)
```

각 디렉터리 README에 책임과 관련 명세 절이 있다.

## 문서 갱신

docx 원본(`docs/specs/original/`)을 개정한 뒤 Markdown을 재생성한다.

```bash
python3 scripts/docs/convert_specs.py
```

pandoc이 필요하다 (`brew install pandoc`).
