# 가짜 PII·secret fixture

redaction 회귀 시험용 fixture다 (D04 §03, D06 §04). **모든 값은 가짜다.** 실제 개인정보나 자격 증명이 아니다.
이 경로는 gitleaks 검사에서 제외된다 (`.gitleaks.toml`).

| 파일 | 내용 |
|---|---|
| `catalog.json` | 범주별 가짜 값 목록. 회귀 시험은 이 값들이 저장 계층·로그 어디에서도 검출되지 않는지 확인한다 |
| `traces_with_pii.json` | span 이름, resource 속성, header(semconv의 string[]), URL·이중 인코딩, DB 문장, 예외 stack, 사용자 속성에 값을 심은 OTLP trace |
| `logs_with_pii.json` | log body(문자열, 3단 중첩 kvlist, 4KiB 경계 뒤 secret)와 속성에 값을 심은 OTLP log |
| `metrics_with_pii.json` | metric point 속성과 exemplar filtered attribute에 값을 심은 OTLP metric |

- 범주: 이메일, 전화번호, 테스트 카드번호, 주민번호 형식, password, Authorization·Cookie header, key 형식 문자열, IPv4·IPv6, 한국어·일본어 이름, query token이 있는 URL, URL 인코딩, 중첩 JSON, multipart, 긴 문자열, SQL literal, stack 속 secret.
- 검색용 표식 `MTXCANARY`가 자유 텍스트 값에 들어 있다. 카드번호·전화번호 같은 형식 값은 표식 없이 값 자체로 검색한다.
- 카드번호는 결제 사업자가 공개한 테스트 번호다. 전화번호 `010-0000-0000`은 미할당 형태이고, 주민번호 `900101-1234567`은 검증 숫자가 맞지 않아 발급될 수 없는 값이다. 이메일은 `example.com`, IP는 문서용 대역(RFC 5737·3849)이다.
- 인코딩 변형(URL 이중 인코딩, `\u0040`)과 한도 경계 뒤 secret은 절단·디코딩 우회를 시험한다.
- 이 fixture가 **통과(디코딩·검증)** 되는지는 decoder 테스트가 확인한다. **제거**되는지는 redaction 구현(Sprint 2)이 확인한다.
