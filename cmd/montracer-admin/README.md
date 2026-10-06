# cmd/montracer-admin

플랫폼 운영자의 break-glass 도구다. 사고 대응(RB03)에서 tenant의 key 목록을 보고, 노출된 key를 즉시 폐기한다.

- 쓰기 소유 데이터: 없음. `api_keys.revoked_at`은 `controldb.KeyStore`를 거쳐 바꾸고, 감사·outbox가 같은 트랜잭션에 남는다
- 동기 의존·장애 동작: 제어 DB(PostgreSQL). 연결 실패면 아무것도 바꾸지 않고 실패한다. 감사 쓰기가 실패하면 폐기도 롤백된다
- 단계: G1 운영 준비 (D04 §01, §11 RB03)
- 명세: D04 §01 관리 접근과 감사, §02 키 관리 · ADR 0033

## 사용

```bash
montracer-admin keys list   --tenant <UUID> --approver <ID> --ticket <INC-…> --reason "<사유>"
montracer-admin keys revoke --tenant <UUID> --key-id <ID> --approver <ID> --ticket <INC-…> --reason "<사유>" --yes
```

- **bastion/session manager에서만 실행한다.** 운영자 ID는 session이 설정한 `MONTRACER_OPERATOR_ID`에서만 읽는다. 인자로는 받지 않고, 없으면 실행하지 않는다.
- 승인자는 운영자와 달라야 한다. 승인자 값은 검증하지 않는 입력이라, 사후에 ticket의 승인 기록과 대조한다(RB03).
- 사유는 10~500자이고, 고객이 감사에서 읽는다는 전제로 쓴다. 고객 데이터·payload를 적지 않는다.
- key ID는 16자리 소문자 hex만 받는다.
- grant는 실행 한 번, 작업 하나, 최대 30분이다. revoke는 `--yes` 없이는 무엇을 할지만 출력한다.
- DB에 닿은 모든 시도는 대상 tenant의 security 감사(`actor_kind='operator'`)에 남는다. 인자·grant 단계에서 거절된 시도는 stderr 구조화 로그(`break-glass rejected`)에 남는다.
- 폐기는 즉시 인증을 막는다. 지금 인증은 제어 DB를 직접 조회한다(cache 없음).

## 환경 변수

`MONTRACER_PG_APP_DSN`(앱 계정, RLS 적용), `MONTRACER_OPERATOR_ID`

## 종료 코드

- 0: 성공, 또는 `--yes` 없는 미리보기
- 1: 실행 실패. not found, grant 만료·범위 밖, DB 오류가 여기에 해당한다
- 2: 인자 오류

## 아직 없는 것

telemetry·감사 본문 break-glass 조회, 승인 요청·기록 서버(승인자 검증), 고객 감사 조회 API, tenant 관리자용 control-api 폐기 (ADR 0033 §4)
