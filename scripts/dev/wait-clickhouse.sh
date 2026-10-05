#!/usr/bin/env bash
# CI·실험용: ClickHouse 컨테이너의 init이 끝나 query 계정이 role을 받은 상태가 안정될 때까지 기다린다.
# 이미지 entrypoint는 init 중 임시 서버를 띄웠다가 재시작하므로, 연속 3회(2초 간격) 성공을 요구한다.
#   usage: wait-clickhouse.sh <container> <query_user> <query_password>
set -euo pipefail
container=$1 user=$2 password=$3
ok=0
for _ in $(seq 1 90); do
  if docker exec "$container" clickhouse-client --user "$user" --password "$password" -q 'SHOW GRANTS' 2>/dev/null \
      | grep -q montracer_ch_query; then
    ok=$((ok + 1))
    [ "$ok" -ge 3 ] && { echo "clickhouse ready"; exit 0; }
  else
    ok=0
  fi
  sleep 2
done
docker logs "$container" | tail -50
exit 1
