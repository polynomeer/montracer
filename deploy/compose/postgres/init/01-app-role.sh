#!/usr/bin/env bash
# 애플리케이션 전용 role 생성 (D02 §11).
# app role은 table owner·superuser·BYPASSRLS가 아니어야 RLS가 우회되지 않는다.
# 컨테이너 최초 초기화 시 1회만 실행된다.
set -euo pipefail

psql -v ON_ERROR_STOP=1 \
  --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v db_name="$POSTGRES_DB" -v app_user="$MONTRACER_APP_USER" -v app_password="$MONTRACER_APP_PASSWORD" <<'SQL'
-- 테이블 권한은 migration이 montracer_rw 그룹에 부여한다 (ADR 0016 §2).
CREATE ROLE montracer_rw NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
CREATE ROLE :"app_user" LOGIN PASSWORD :'app_password'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS IN ROLE montracer_rw;
GRANT CONNECT ON DATABASE :"db_name" TO :"app_user";
SQL
