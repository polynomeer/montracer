# PS-0004: 로컬 stack의 port 충돌·다른 프로젝트 Kafka 오접속·OOM kill

- 날짜: 2026-10-03 ~ 2026-10-05
- 영역: 로컬 환경 (`deploy/compose`)
- 영향: 개발 중단. 다른 프로젝트의 Kafka에 topic·데이터를 쓸 뻔한 위험 — P1
- 수정: `23d8364`, `5176e30`, `52825a7`
- 관련: ADR 0020 (로컬 Kafka 포트 안전장치), `deploy/compose/README.md`

## 증상

1. host에서 ClickHouse 포트로 접속되지 않았다.
2. Kafka 기본 host 포트 19092가 같은 머신의 다른 프로젝트와 충돌했다.
3. 포트를 19192로 바꾼 뒤에도, 낡은 `.env`가 19092(다른 프로젝트의 Kafka)를 가리켜 거기에 topic을 만들 뻔했다.
4. Kafka·ClickHouse 컨테이너가 exit 137(커널 OOM kill)로 반복 종료되었다.

## 원인

1. ClickHouse 이미지가 컨테이너 loopback에만 listen해서 port mapping이 닿지 않았다.
2. 3. 1xxxx 대역도 다른 프로젝트와 겹칠 수 있었고, 환경 변수 파일은 compose 설정과 따로 낡는다.
4. Docker VM을 다른 프로젝트와 나눠 쓰는데, Kafka JVM 기본 heap과 ClickHouse 기본 캐시(mark cache 5GiB, 서버 상한은 VM의 90%)가 VM 메모리를 넘었다.

## 해결

- ClickHouse `config.d`에서 `listen_host 0.0.0.0`을 지정했다. host 노출은 compose의 `127.0.0.1` 바인딩으로 제한한다.
- Kafka host 포트를 19192로 옮겼다.
- `check-kafka-port.sh`: `KAFKA_PORT`가 montracer compose Kafka의 실제 포트가 아니면 중단한다.
- lite 프로필 메모리 상한: Kafka heap 512MiB, ClickHouse `max_server_memory_usage` 1.5GB, mark cache 256MiB, uncompressed cache 끔. production 설정과는 무관하다.

## 재발 방지

- `scripts/dev/check-kafka-port.sh`가 `make migrate-kafka`·`make test-integration` 앞에서 실행된다.
- 로컬 통합 검증이 자원 때문에 실패하면 CI 통합 job을 신뢰할 수 있는 검증으로 삼는다.

## 교훈

로컬 stack은 **같은 머신의 다른 시스템과 공존**한다고 가정한다. 외부 시스템에 쓰는 명령(topic 생성, migration)은 대상이 정말 우리 것인지 실행 전에 확인한다.
