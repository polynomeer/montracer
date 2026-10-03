#!/usr/bin/env bash
# Collector 이미지는 distroless라 compose healthcheck를 쓸 수 없어 host에서 health endpoint를 확인한다.
set -euo pipefail
port=$(awk -F= '$1=="OTELCOL_HEALTH_PORT" {print $2}' .env 2>/dev/null)
port=${port:-13133}
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${port}/" >/dev/null 2>&1; then
    echo "otel-collector healthy (:${port})"
    exit 0
  fi
  sleep 1
done
echo "otel-collector health check 실패 — make logs SERVICE=otel-collector" >&2
exit 1
