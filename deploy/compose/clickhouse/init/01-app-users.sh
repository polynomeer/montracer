#!/usr/bin/env bash
# 앱 계정 생성 (ADR 0018). 테이블 권한·row policy·settings profile은 migration이 role에 부여한다.
#   ingest: 수집 worker, 원본 테이블 INSERT만
#   query : query service, SELECT만 + tenant row policy + 실행 예산
#   rollup: metric window 집계 job, metric_points 읽기 + metric_1m 쓰기 (ADR 0026)
# 멱등이며 CLICKHOUSE_ALWAYS_RUN_INITDB_SCRIPTS=1로 매 기동 시 실행된다. 비밀번호 변경도 매번 반영한다.
set -euo pipefail

# GRANT·ALTER USER는 식별자 parameter를 지원하지 않으므로 이름을 검증한 뒤 넣는다.
for name in "$MONTRACER_CH_INGEST_USER" "$MONTRACER_CH_QUERY_USER" "$MONTRACER_CH_ROLLUP_USER"; do
  [[ "$name" =~ ^[a-z_][a-z0-9_]{0,62}$ ]] || { echo "invalid clickhouse user name: $name" >&2; exit 1; }
done
ingest="$MONTRACER_CH_INGEST_USER"
query="$MONTRACER_CH_QUERY_USER"
rollup="$MONTRACER_CH_ROLLUP_USER"

# 비밀번호는 문자열 parameter로 넘겨 SQL에 이어 붙이지 않는다.
clickhouse client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --multiquery \
  --param_ingest_pw="$MONTRACER_CH_INGEST_PASSWORD" --param_query_pw="$MONTRACER_CH_QUERY_PASSWORD" \
  --param_rollup_pw="$MONTRACER_CH_ROLLUP_PASSWORD" <<SQL
CREATE ROLE IF NOT EXISTS montracer_ch_ingest;
CREATE ROLE IF NOT EXISTS montracer_ch_query;
CREATE ROLE IF NOT EXISTS montracer_ch_rollup;
CREATE USER IF NOT EXISTS ${ingest} IDENTIFIED WITH sha256_password BY {ingest_pw:String} DEFAULT DATABASE montracer;
CREATE USER IF NOT EXISTS ${query} IDENTIFIED WITH sha256_password BY {query_pw:String} DEFAULT DATABASE montracer;
CREATE USER IF NOT EXISTS ${rollup} IDENTIFIED WITH sha256_password BY {rollup_pw:String} DEFAULT DATABASE montracer;
ALTER USER ${ingest} IDENTIFIED WITH sha256_password BY {ingest_pw:String};
ALTER USER ${query} IDENTIFIED WITH sha256_password BY {query_pw:String};
ALTER USER ${rollup} IDENTIFIED WITH sha256_password BY {rollup_pw:String};
GRANT montracer_ch_ingest TO ${ingest};
GRANT montracer_ch_query TO ${query};
GRANT montracer_ch_rollup TO ${rollup};
ALTER USER ${ingest} DEFAULT ROLE montracer_ch_ingest;
ALTER USER ${query} DEFAULT ROLE montracer_ch_query;
ALTER USER ${rollup} DEFAULT ROLE montracer_ch_rollup;
SQL
