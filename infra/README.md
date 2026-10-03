# infra

네트워크와 관리형 서비스(Kafka, ClickHouse, PostgreSQL, object store) IaC.
클라우드 공급자와 관리형 서비스 선택은 작업계획서 §8 결정 사항이며 ADR로 확정한 뒤 작성한다.

- production 권한은 CI workload identity로만 부여하고 개인 장기 credential을 쓰지 않는다 (D06 §07).
- 고객 유료 production은 3 AZ Kafka, ClickHouse 복제, PostgreSQL HA를 전제로 한다 (D01 §02, D04 §05).
