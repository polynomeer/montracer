# Montracer

서비스 상태 파악부터 요청 단위 원인 조사, JVM 내부 진단, 사용자 경험 확인까지 연결하는 B2B 관측(APM) 플랫폼.

> 현재 상태: **설계 완료 · 구현 착수 전.** 실행 가능한 코드는 아직 없다.

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
├── docs/
│   ├── specs/                # 설계 명세 Markdown + assets + original(docx 원본)
│   ├── plan/                 # 작업계획서, 요구사항 registry
│   ├── adr/                  # 결정 기록
│   └── runbooks/             # 운영 절차
└── scripts/docs/             # 문서 변환 스크립트
```

구현이 시작되면 D06 §10~11의 모노레포 구조(`apps/web`, `cmd/*`, `internal/*`, `api/*`, `migrations/*`, `deploy/*`, `tests/*`)가 추가된다.

## 문서 갱신

docx 원본(`docs/specs/original/`)을 개정한 뒤 Markdown을 재생성한다.

```bash
python3 scripts/docs/convert_specs.py
```

pandoc이 필요하다 (`brew install pandoc`).
