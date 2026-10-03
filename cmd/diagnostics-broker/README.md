# cmd/diagnostics-broker

승인된 원격 진단 job(thread dump, probe)을 agent의 outbound channel로 전달. 승인·TTL·nonce·scope·boot_id 검증, 임의 명령 실행 금지.

- 쓰기 소유 데이터: 승인된 job과 agent channel 상태
- 동기 의존·장애 동작: 만료 후 전송·실행 금지. server·agent 양쪽 kill switch.
- 단계: G1 제한 Beta, G2 GA (F16)
- 명세: D03 §05, §08 · ADR 011
