# deploy/compose — 로컬 stack

| 파일 | 내용 |
|---|---|
| `versions.env` | 이미지 digest 고정 (ADR 0013) |
| `compose.lite.yaml` | PostgreSQL 17, Kafka 4.3 (KRaft 단일 broker), ClickHouse 26.8, OTel Collector |
| `postgres/init/` | 최초 기동 시 app role 생성 (NOSUPERUSER·NOBYPASSRLS) |
| `clickhouse/config.d/` | 로컬 전용 ClickHouse 설정 |
| `clickhouse/init/` | 매 기동 시 ingest·query 계정 생성 (멱등, ADR 0018) |
| `collector/config.yaml` | OTLP 수신 → debug exporter (ingress 구현 전) |

## 사용

```bash
make bootstrap          # .env 생성
make up PROFILE=lite    # 기동 후 healthy까지 대기
make ps                 # 상태
make logs               # 로그
make migrate            # PostgreSQL·ClickHouse schema (+ make migrate-kafka: topic)
make dev                # ingress·worker·query-api·control-api 실행 (Ctrl-C 종료)
make seed SCENARIO=checkout   # 다른 터미널: demo tenant 2개·key·checkout 시나리오 적재 (재실행해도 logical 중복 없음)
make smoke              # trace 조회·tenant 격리·감사·metric oracle(요청 1,000·오류 20)·metric 사전 확인
make down               # 종료 (데이터 유지)
make clean-data         # 데이터 볼륨 삭제 (확인 프롬프트)
```

## 접속 (기본 host 포트, `.env`에서 변경)

기본 포트(5432, 9092, 8123, 4317…)와 충돌하지 않도록 1xxxx 대역을 쓴다. 모두 127.0.0.1에만 바인딩.

| 서비스 | 주소 |
|---|---|
| PostgreSQL | `localhost:15432` (app: `montracer_app`, owner: `montracer_admin`) |
| Kafka | `localhost:19192` (컨테이너 내부 `kafka:29092`) |
| ClickHouse | HTTP `localhost:18123`, native `localhost:19000`. 계정: `montracer_admin`(migration), `montracer_ingest`(INSERT만), `montracer_query`(SELECT만 + tenant row policy) |
| OTLP | gRPC `localhost:14317`, HTTP `localhost:14318`, health `localhost:13133` (Collector, 아직 debug exporter) |
| `make dev` 서비스 | ingress OTLP/HTTP `127.0.0.1:18318`, query-api `:18080`, control-api `:18081`, 운영 지표 `:19464~19467` |

`make seed`의 demo key·상태는 `.seed/demo.json`(git 밖)에 둔다. pepper·cursor key는 `scripts/dev/local-key.sh`가 고정 문구에서 만드는 로컬 전용 값이다(production은 secret manager). seed·smoke 도구는 localhost가 아닌 주소를 거절한다.

## production과 다른 점

단일 broker RF1, 복제 없는 ClickHouse, HA 없는 PostgreSQL. 이 stack으로 측정한 수치는 SLO 근거가 될 수 없다 (D01 §02).
Kafka 토픽 자동 생성은 꺼져 있다 — 토픽은 코드/migration에서 명시적으로 만든다.
`full` profile(object store emulator, IdP, 진단 agent 샘플, synthetic runner)은 추후 추가한다.
