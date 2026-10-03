# deploy

| 경로 | 용도 | 명세 |
|---|---|---|
| `compose/` | 로컬 stack. `lite`: PostgreSQL, Kafka 단일 broker, ClickHouse, Collector, API, UI / `full`: + object store emulator, IdP, 진단 agent 샘플, synthetic runner | D06 §10~11 |
| `helm/` | staging·production Kubernetes 배포 | D04 §09 |

- 이미지는 digest로 고정한다. `latest`·`main` 태그 금지.
- runner에 Docker socket을 mount하지 않는다.
- 로컬 seed key는 localhost 전용, production image에 포함하지 않는다.
