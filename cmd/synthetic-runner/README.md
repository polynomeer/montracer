# cmd/synthetic-runner

API·Browser Synthetic 테스트 실행. SSRF·DNS rebinding 차단, 플랫폼 실패와 고객 장애 구분.

- 쓰기 소유 데이터: synthetic run 결과·artifact
- 동기 의존·장애 동작: runner 중단은 고객 availability와 별도 표시, 이중 청구 금지.
- 단계: G1 (F20a: HTTP·TCP·TLS·DNS 점검, 공용 runner 2 AZ — ADR 0071), G2 (F20b: multi-step·사설 runner·다지역), G3 (F21)
- 명세: D03 §11 · D04 §14
