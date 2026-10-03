#!/usr/bin/env bash
# make doctor: 로컬 개발 환경 점검 (D06 §10~11).
# 권고 자원: 8 CPU, 16GB RAM, 50GB 여유 disk. 버전 고정은 ADR 확정 후 .tool-versions로 관리한다.
set -u

fail=0
ok()   { printf '  \033[32mOK\033[0m   %s\n' "$1"; }
warn() { printf '  \033[33mWARN\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=1; }

echo "도구"
check_tool() {
  local name=$1 hint=$2
  if command -v "$name" >/dev/null 2>&1; then
    ok "$name: $("$name" ${3:---version} 2>&1 | head -1)"
  else
    bad "$name 없음 — $hint"
  fi
}
check_tool docker "Docker Desktop 또는 호환 container runtime 설치"
check_tool go     "https://go.dev/dl/" version
check_tool node   "Node LTS 설치"
check_tool pnpm   "corepack enable && corepack prepare pnpm --activate"
check_tool pandoc "brew install pandoc (문서 변환용)"
# 고정 버전 확인 (ADR 0013)
if command -v node >/dev/null 2>&1; then
  want_node=$(cat .nvmrc); have_node=$(node --version | sed 's/^v//')
  [ "${have_node%%.*}" = "${want_node%%.*}" ] && ok "Node major ${have_node%%.*} (고정 $want_node)" \
    || warn "Node $have_node — 고정 버전은 $want_node (.nvmrc). nvm/fnm/volta로 전환 권장"
fi
if command -v go >/dev/null 2>&1; then
  ok "Go toolchain: go.mod의 toolchain($(awk '/^toolchain/ {print $2}' go.mod))을 GOTOOLCHAIN=auto로 자동 사용"
fi
if command -v docker >/dev/null 2>&1 && ! docker info >/dev/null 2>&1; then
  bad "docker daemon이 실행 중이 아님"
fi

echo "자원"
case "$(uname -s)" in
  Darwin) cpu=$(sysctl -n hw.ncpu); mem_gb=$(( $(sysctl -n hw.memsize) / 1024 / 1024 / 1024 )) ;;
  *)      cpu=$(nproc); mem_gb=$(( $(awk '/MemTotal/ {print $2}' /proc/meminfo) / 1024 / 1024 )) ;;
esac
disk_gb=$(df -Pk . | awk 'NR==2 {print int($4/1024/1024)}')
[ "$cpu" -ge 8 ]      && ok "CPU ${cpu}개"        || warn "CPU ${cpu}개 (권고 8)"
[ "$mem_gb" -ge 16 ]  && ok "RAM ${mem_gb}GB"     || warn "RAM ${mem_gb}GB (권고 16GB)"
[ "$disk_gb" -ge 50 ] && ok "여유 disk ${disk_gb}GB" || warn "여유 disk ${disk_gb}GB (권고 50GB)"

echo "포트 (lite profile)"
# PostgreSQL 5432, Kafka 9092, ClickHouse 8123/9000, OTLP 4317/4318
for port in 5432 9092 8123 9000 4317 4318; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    warn "포트 $port 사용 중 — 기존 프로세스 확인"
  else
    ok "포트 $port 사용 가능"
  fi
done

echo "설정"
[ -f .env ] && ok ".env 존재" || warn ".env 없음 — make bootstrap 실행"

exit $fail
