#!/usr/bin/env bash
# make dev: 로컬 lite stack 위에서 ingress·worker(ingest,rollup)·query-api·control-api를 띄운다 (D06 §10~11).
# 환경은 Makefile이 넘긴다(DEV_* 변수). Ctrl-C로 모두 종료한다. production 설정이 아니다.
#
# 포트 (모두 127.0.0.1)
#   ingress OTLP/HTTP  ${DEV_INGRESS_PORT}   (Collector의 14318과 별개)
#   ingress OTLP/gRPC  ${DEV_INGRESS_GRPC_PORT} (Collector의 14317과 별개)
#   query-api          ${DEV_QUERY_PORT}
#   control-api        ${DEV_CONTROL_PORT}
#   운영 지표          19464(ingress) 19465(worker) 19466(query) 19467(control)
set -euo pipefail

for v in DEV_PG_APP_DSN DEV_CH_INGEST_DSN DEV_CH_QUERY_DSN DEV_CH_ROLLUP_DSN DEV_KAFKA_BROKERS \
  DEV_PEPPER_HEX DEV_CURSOR_KEY_HEX DEV_INGRESS_PORT DEV_INGRESS_GRPC_PORT DEV_QUERY_PORT DEV_CONTROL_PORT; do
  if [ -z "${!v:-}" ]; then
    echo "make dev: $v 가 비어 있다 — make dev로 실행한다" >&2
    exit 2
  fi
done

mkdir -p bin
echo "build: ingress worker query-api control-api → bin/"
go build -o bin/ ./cmd/ingress ./cmd/worker ./cmd/query-api ./cmd/control-api

pids=()
cleanup() {
  trap - EXIT INT TERM
  # 각 run은 (binary | sed) subshell이다. subshell만 죽이면 binary가 남으므로 자식부터 내린다.
  for p in ${pids[@]+"${pids[@]}"}; do pkill -TERM -P "$p" 2>/dev/null || true; kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# run <이름> <env...> -- <binary>: 출력 줄마다 이름을 붙인다. 하나가 죽으면 전체를 내린다.
run() {
  local name=$1
  shift
  local envs=()
  while [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift
  ( env "${envs[@]}" "$@" 2>&1 | sed -u "s/^/[$name] /"; echo "[$name] exited" ) &
  pids+=($!)
}

run ingress MONTRACER_INGRESS_ADDR="127.0.0.1:${DEV_INGRESS_PORT}" MONTRACER_INGRESS_GRPC_ADDR="127.0.0.1:${DEV_INGRESS_GRPC_PORT}" \
  MONTRACER_PG_APP_DSN="$DEV_PG_APP_DSN" \
  MONTRACER_KEY_PEPPER_HEX="$DEV_PEPPER_HEX" MONTRACER_KAFKA_BROKERS="$DEV_KAFKA_BROKERS" \
  MONTRACER_METRICS_ADDR=127.0.0.1:19464 -- bin/ingress
run worker MONTRACER_WORKER_ROLES=ingest,rollup MONTRACER_KAFKA_BROKERS="$DEV_KAFKA_BROKERS" \
  MONTRACER_CH_INGEST_DSN="$DEV_CH_INGEST_DSN" MONTRACER_CH_ROLLUP_DSN="$DEV_CH_ROLLUP_DSN" MONTRACER_CH_INSERT_QUORUM=0 \
  MONTRACER_METRICS_ADDR=127.0.0.1:19465 -- bin/worker
run query MONTRACER_QUERY_ADDR="127.0.0.1:${DEV_QUERY_PORT}" MONTRACER_PG_APP_DSN="$DEV_PG_APP_DSN" \
  MONTRACER_KEY_PEPPER_HEX="$DEV_PEPPER_HEX" MONTRACER_CH_QUERY_DSN="$DEV_CH_QUERY_DSN" \
  MONTRACER_METRICS_ADDR=127.0.0.1:19466 -- bin/query-api
run control MONTRACER_CONTROL_ADDR="127.0.0.1:${DEV_CONTROL_PORT}" MONTRACER_PG_APP_DSN="$DEV_PG_APP_DSN" \
  MONTRACER_KEY_PEPPER_HEX="$DEV_PEPPER_HEX" MONTRACER_CURSOR_KEY_HEX="$DEV_CURSOR_KEY_HEX" \
  MONTRACER_METRICS_ADDR=127.0.0.1:19467 -- bin/control-api

echo "running: ingress http://127.0.0.1:${DEV_INGRESS_PORT} grpc 127.0.0.1:${DEV_INGRESS_GRPC_PORT}  query http://127.0.0.1:${DEV_QUERY_PORT}  control http://127.0.0.1:${DEV_CONTROL_PORT}"
echo "다른 터미널에서: make seed SCENARIO=checkout && make smoke   (종료: Ctrl-C)"
# 하나라도 끝나면 나머지를 내린다(조용히 일부만 도는 상태를 만들지 않는다).
# macOS 기본 bash 3.2에는 wait -n이 없어 주기적으로 확인한다.
while :; do
  for p in "${pids[@]}"; do
    if ! kill -0 "$p" 2>/dev/null; then
      echo "a service exited — stopping the rest" >&2
      exit 1
    fi
  done
  sleep 1
done
