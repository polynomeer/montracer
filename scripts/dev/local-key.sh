#!/usr/bin/env bash
# 로컬 개발·CI 전용 fake key(hex)를 고정 문구에서 만든다: local-key.sh <이름>
# make dev·seed·smoke와 CI가 같은 값을 쓰게 하는 단일 원천이다. production에서는 secret manager가 주입한다.
set -euo pipefail
name=${1:?usage: local-key.sh <name>}
printf '%s' "local-dev-only-${name}-not-a-secret" | od -An -tx1 | tr -d ' \n'
