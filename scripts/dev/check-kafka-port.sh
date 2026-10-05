#!/usr/bin/env bash
# Kafka를 쓰는 make target의 안전장치: KAFKA_PORT가 montracer compose의 Kafka가 실제로 연 host 포트인지 확인한다.
# 다른 로컬 프로젝트의 Kafka(예: 19092)에 topic을 만들거나 데이터를 쓰는 사고를 막는다 (ADR 0020 §6).
#   usage: check-kafka-port.sh <KAFKA_PORT>
set -euo pipefail
want=${1:-}
cid=$(docker ps -q --filter label=com.docker.compose.project=montracer --filter label=com.docker.compose.service=kafka | head -1)
actual=""
if [ -n "$cid" ]; then
  actual=$(docker port "$cid" 9092/tcp 2>/dev/null | head -1 | awk -F: '{print $NF}')
fi
if [ -z "$actual" ]; then
  echo "montracer Kafka가 실행 중이 아니다 — make up PROFILE=lite" >&2
  exit 1
fi
if [ "$want" != "$actual" ]; then
  echo "KAFKA_PORT=$want 이지만 montracer Kafka는 host 포트 $actual 에 있다. .env의 KAFKA_PORT·KAFKA_BROKERS를 $actual 로 맞춰라." >&2
  echo "(다른 프로젝트의 Kafka에 쓰는 것을 막기 위해 중단한다)" >&2
  exit 1
fi
