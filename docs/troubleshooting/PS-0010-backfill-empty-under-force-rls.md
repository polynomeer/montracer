# PS-0010: migration backfill이 FORCE RLS 표에서 오류 없이 0행을 옮김

- 날짜: 2026-10-09 (발견) / 2026-10-09 (해결)
- 영역: controldb (migration)
- 영향: 기존 monitor가 평가되지 않음 — 고객 경보가 소리 없이 꺼짐(가용성). 심각도 P1. 출시 전 리뷰에서 발견해 운영 영향은 없다
- 수정: E05 단계 B2 PR (alert-worker, migration 00008)
- 관련: ADR 0016 §1, ADR 0051 §1, D06 §07

## 증상

migration 00008은 이미 있는 monitor마다 평가 일정(`monitor_schedule`) 행을 만든다(`INSERT … SELECT … FROM monitors`). superuser가 아닌 owner 계정으로 돌리면 이 INSERT가 **오류 없이 0행**을 넣는다. 그러면 기존 monitor는 누가 정의를 수정하기 전까지 평가되지 않는다.

로컬과 CI는 공식 PostgreSQL 이미지의 `POSTGRES_USER`(superuser)로 migration을 돌려서 증상이 보이지 않았다.

## 발견 경위

PR 전 spec-reviewer가 지적했다(P1). ADR 0016에는 "migration은 owner 계정으로 실행한다"와 "FORCE RLS에서는 owner도 막힌다"가 함께 적혀 있었다. 두 문장을 합치면 FORCE RLS 표에서 읽는 backfill은 빈 결과가 된다. 확인 방법은 다음과 같았다.
1. 로컬 PG에서 superuser가 아닌 owner(`mig_owner`)의 DB를 만든다.
2. 00007까지 올리고 monitor 2개를 넣는다.
3. owner로 `SELECT count(*) FROM monitors`를 실행하면 0이다.

## 원인

1. `monitors`는 `ENABLE`+`FORCE ROW LEVEL SECURITY`다(00007). FORCE는 표 owner에게도 정책을 적용한다.
2. 정책은 `tenant_id = app_tenant_id()`다. migration 세션에는 `app.tenant_id`가 없어 `app_tenant_id()`가 NULL이고, 모든 행이 걸러진다.
3. superuser는 RLS를 항상 우회하므로, superuser로만 시험하면 backfill이 정상으로 보인다.
4. 빈 SELECT는 오류가 아니므로 migration은 성공으로 끝난다.

## 해결

- backfill 동안만 `ALTER TABLE monitors NO FORCE ROW LEVEL SECURITY`로 owner가 읽게 하고, 끝나면 다시 FORCE한다. goose가 파일 하나를 한 트랜잭션으로 돌리므로, 중간에 실패하면 NO FORCE도 함께 되돌려진다.
- 새 표(`monitor_schedule` 등)의 FORCE도 backfill 뒤에 건다. 그 전에는 owner가 tenant 정책 없이 INSERT할 수 있다.
- **완전성 검사:** `monitors` 행 수와 `monitor_schedule` 행 수가 다르면 `RAISE EXCEPTION`으로 migration을 실패시킨다. 빈 backfill이 다시 조용히 지나가지 않는다.
- 버린 대안
  - BYPASSRLS migration 계정: ADR 0016이 피한 권한이다.
  - tenant마다 `set_config`로 반복 INSERT: tenant 목록 자체(`tenants`)도 RLS라 같은 문제가 반복된다.

## 재발 방지

- CI `PostgreSQL migrate — superuser가 아닌 owner 계정, 기존 정의가 있는 backfill`:
  1. superuser가 아닌 owner로 up → down을 돌린다.
  2. monitor 2개를 넣는다.
  3. 다시 up한 뒤 schedule이 2행인지 확인한다.
- migration 00008의 행 수 검사(`RAISE EXCEPTION`)

## 교훈

- RLS 표를 읽는 migration(backfill·데이터 이전)은 superuser 시험만으로 검증되지 않는다. ADR 0016의 계정 구성(superuser가 아닌 owner)으로 돌리는 시험이 필요하다.
- backfill에는 "옮긴 수 = 원천 수" 검사를 붙인다. 빈 결과는 오류가 아니어서 다른 방법으로는 드러나지 않는다.
