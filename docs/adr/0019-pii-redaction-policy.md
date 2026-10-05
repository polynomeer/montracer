# ADR 0019: PII redaction 기본 정책과 실패 처리

- 상태: 승인
- Owner: Security
- 승인자: polynomeer
- 날짜: 2026-10-05 (제안) / 2026-10-05 (결정)
- 관련: F01, F07, E02 · D04 §03 · D02 §04, §05 · D03 §02 · CLAUDE.md 변경 불가 계약 3

## 배경

D04 §03은 다음을 요구한다.

- **최소 수집**: HTTP header는 기본 거부(deny)하고 allowlist만 통과시킨다. authorization·cookie·password·token·secret·개인식별번호는 수집하지 않는다. URL query string은 지우고, DB 문장은 parameterized 형태만 남긴다.
- **패턴 치환**: 이메일·전화번호 등은 "보완 수단이며 탐지 누락을 전제"로 한다.
- **처리 순서**: 서버 ingress가 Kafka·DLQ·archive·live stream·오류 log에 쓰기 **전에** 2차로 강제 적용한다.
- **실패 처리**: redaction이 실패하면 record를 저장하지 않고 영구 거절 건수로 보고한다. 원본을 몰래 보관하지 않는다.
- **정책 구성**: key allowlist·denylist, 값 길이 제한, pattern replacement, 선택적 HMAC tokenization, policy_version으로 이뤄진다.

명세가 정하지 않은 것은 구체적인 key 목록, 패턴, 치환 표기, 실패 단위다.

## 결정

### 1. key 규칙 (1차 방어, 필드 삭제)

| 규칙 | 기본 목록 |
|---|---|
| deny 토큰 | 인증 정보: `authorization, auth, cookie, password, passwd, pwd, passphrase, secret, token, apikey, accesskey, privatekey, secretkey, credential(s), session, sessionid, jwt, otp, pin, cvv, cvc` · 식별자: `user, username, enduser, customer, email, phone, mobile, ssn, rrn, card, creditcard, pan, iban, passport` |
| header (`http.request.header.*`, `http.response.header.*`) | 기본 삭제. 통과: `content-type, content-length, accept, accept-encoding, user-agent, x-request-id, traceparent, tracestate` |
| 통째로 삭제 | `url.query`, 접두어 `db.query.parameter.*`·`db.statement.parameter.*`(바인딩된 SQL 파라미터 값) |
| deny 예외 (값에는 패턴 치환) | 접두어 `user_agent.` (header allowlist의 `user-agent`와 일관) |

**key 정규화 방법**

1. camelCase 경계에서 나눈다: `userEmail` → `user`, `email`. `HTTPAuthorization` → `http`, `authorization`.
2. 소문자로 바꾼다.
3. 구분자 `. _ - : /`와 공백으로 나눈다.
4. 각 토큰과, 인접한 두 토큰을 붙인 형태를 deny 목록과 비교한다. 예: `api_key` → `apikey`, `credit_card` → `creditcard`.

- 중첩된 map(구조화 log body 등)에도 같은 key 규칙을 적용한다.
- `user`·`session` 토큰은 범위가 넓다. 예를 들어 `user_agent.original`, RUM의 `session.id`도 삭제된다. "고객 식별자 기본 미수집"(D04 §03 표)을 우선했다. RUM(F19)에서 세션 ID가 필요해지면 tenant HMAC tokenization과 함께 이 ADR을 다시 연다.

### 2. 값 규칙

| 대상 key | 처리 |
|---|---|
| `url.full, url.original, http.url, http.target, url.path` | query·fragment·userinfo를 제거한 뒤 패턴 치환 |
| `db.query.text, db.statement` | 문자열·숫자 literal을 `?`로 치환한 뒤 패턴 치환 |
| `client.address, source.address, http.client_ip, net.peer.ip, network.peer.address, peer.ip, http.request.header.x-forwarded-for` | 목록(XFF)·port·bracket·zone을 풀어 주소마다 처리한다. **공인 IP만** IPv4 /24, IPv6 /48로 축약하고, 사설·loopback·link-local은 그대로 둔다. 해석할 수 없는 값은 삭제한다 |
| 그 외 모든 문자열 (span 이름, status message, event 이름, 속성, log body·severity text·event name) | 패턴 치환. 자유 텍스트 속 공인 IP도 같은 규칙으로 축약한다 |
| URL·SQL·IP key인데 값이 문자열이 아닌 경우 (map·bytes) | 해당 규칙을 적용할 수 없으므로 삭제 |

- **IP 판단 근거:** D04 §03 표의 "IP 주소: 제거 또는 축약"은 최종 사용자를 식별하는 공인 주소를 대상으로 본다. 노드·DB·서비스의 사설 주소는 JVM·K8s 진단과 의존성 지도에 필요한 인프라 식별자라 남긴다.
- **SQL literal 처리**

  | 대상 | 결과 |
  |---|---|
  | `'…'`(`''`·`\'` escape 포함), 닫히지 않은 `'…`(SDK가 자른 문장) | `?` |
  | `$tag$…$tag$`, `0x` hex, 숫자 | `?` |
  | 식별자 형태가 아닌 `"…"`(MySQL 문자열) | `?` |
  | placeholder(`$1`·`:name`·`?`), 식별자 안의 숫자(`table1`), 식별자 형태의 `"…"`(PostgreSQL) | 보존 |

### 3. 패턴 (보완 수단)

- **탐지 대상**
  - JWT, Montracer key 형식(`mti_`·`mta_`)
  - Authorization 값(scheme과 credential 모두)
  - 인증 scheme 토큰: Bearer·Token·Digest·ApiKey, base64 8자 이상의 Basic
  - key=value secret: 따옴표 값(`{"password":"…"}`, `password='…'`)도 포함한다. key는 남기고 값만 치환한다.
  - 이메일
  - 주민·외국인등록번호: 앞 6자리가 유효한 YYMMDD이고 성별 자리가 1~8일 때만. epoch ms 같은 숫자가 지워지지 않게 하기 위해서다.
  - 미국 SSN 형식, 한국 휴대전화
  - 카드번호
- **카드번호 판정**: 공백·`-`로 이어진 숫자 묶음에서, 이어진 묶음 조합의 자릿수가 13~19이고 첫 자리(BIN)가 3~6이며 Luhn을 통과하면 치환한다. 앞에 다른 숫자가 붙은 경우("qty 2 4111 …")도 잡는다. 구분자 없는 13~19자리 ID가 우연히 조건을 만족할 확률은 약 4%다(BIN 40% × Luhn 10%). 이 정도 오탐은 감수한다.
- **인코딩 우회 대응**: % 인코딩을 최대 3회 연쇄로 푼다. 유효한 `%XX`만 풀고, `+`는 공백으로 바꾸지 않는다. 각 단계에서 `\u0040`도 풀어 본다. 풀어 본 문자열에서 원문보다 많이 탐지되면, 원문 일부를 남기지 않도록 **값 전체**를 `[REDACTED:encoded]`로 바꾼다.
- **멱등성**: 같은 record를 다시 redaction해도 결과가 같다(재전송 대비). 이미 축약된 CIDR은 그대로 둔다.
- **치환 표기**: `[REDACTED:<kind>]`. 원문 일부(앞뒤 몇 글자 등)를 남기지 않는다. 치환 건수는 kind별로 세어 metric으로 내보낸다.

### 4. 실패 처리

- redaction은 record 단위로 격리한다. record는 span, log record, metric data point다.
- 처리 중 panic이 나면 그 record를 **제거**하고 `redaction_failed` 거절 건수로만 보고한다.
- recover 값과 원문은 결과·오류·로그 어디에도 남기지 않는다.
- resource나 scope 속성 처리가 실패하면, 그 아래 record 전체를 실패로 본다. 이 속성은 모든 envelope에 복제되기 때문이다.

### 5. 알려진 한계

- 자유 텍스트 안의 사람 이름(예: log body 문장 속 이름)은 패턴으로 탐지할 수 없다. 이런 이름은 아래 두 가지로만 막는다.
  - key 규칙: `enduser.*`, 구조화 body의 `user` 같은 key는 삭제된다.
  - log body 수집 정책(D04 §03): 고객이 본문 수집을 켤 때 데이터 분류를 지정한다.
- 회귀 테스트는 이 항목을 "알려진 한계"로 명시해 둔다. 숨겨서 통과시키지 않는다.
- 탐지하지 못하는 인코딩: HTML entity(`&#64;`), 전각 문자(`＠`), base64로 감싼 값.
- 문자열 log body 안의 JSON은 파싱하지 않는다. key=value 패턴으로만 잡는다.
- 4부분 버전 문자열(예: `1.4.2.0`)이 공인 IPv4로 보여 축약될 수 있다.
- **값 길이 제한**(D04 §03 정책 구성 요소)은 redaction이 아니라 decoder의 record 검증이 맡는다(속성 값 4KiB, log body 32KiB, ADR 0017).
- **metric series identity:** 속성을 지우거나 치환하면 서로 다른 series가 같은 속성 집합으로 합쳐질 수 있다. 정규화 worker의 stream identity·충돌 처리(D02 §07, §21)에서 다룬다.
- 아래는 스키마·식별 정보라 치환하지 않는다: metric 이름·설명·단위, scope 이름·버전, W3C trace state.
- HMAC tokenization(tenant별 key)은 아직 구현하지 않았다. 고객 식별자를 가명으로 보존해야 하는 기능에서 추가한다.

### 5-1. 성능 (2026-10-05, Apple Silicon 1 core)

소문자 키워드 사전 필터를 둔 뒤 32KiB 값 하나의 처리 속도다.

| 입력 | 처리 속도 |
|---|---|
| 일반 텍스트 | 121MB/s |
| 숫자가 많은 텍스트 | 4.3MB/s |
| % 인코딩이 많은 텍스트 | 1.5MB/s |

숫자 패턴 5종과 인코딩 해석본(최대 4개)이 비용의 대부분이다. ingress 부하 시험(D06 §03)에서 CPU 예산과 비교하고, 넘으면 패턴을 단일 패스로 합치는 최적화를 한다.

### 6. 정책 버전

- `Policy.Version`을 envelope의 `policy_version`으로 기록한다 (D02 §05).
- 정책을 바꾸면 버전을 올리고, 이 ADR과 회귀 fixture를 같은 PR에서 갱신한다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 1차 방어 | key deny + header 기본 deny | 패턴만 사용: 탐지 누락을 전제로 한 보완 수단이라 1차 방어가 될 수 없다 (D04 §03) |
| 실패 단위 | record | 요청 전체 거절: 정상 record까지 잃는다. 부분 치환 후 저장: 원문이 섞일 위험이 있다 |
| 치환 표기 | 종류만 표시 | 부분 마스킹(앞 4자리 유지 등): 원문 일부가 남는다 |

## 결과

- 회귀 테스트 `TestPIIFixturesAreRedacted`: catalog의 모든 항목에 기대 결과(사라져야 함 또는 알려진 한계)를 반드시 지정해야 통과한다. 새 fixture 항목을 추가할 때 기대를 빠뜨리지 못한다.
- ingress(Sprint 2)는 이 단계를 거치지 않고 Kafka에 쓸 수 없다. 처리 순서는 검증 → tenant 주입 → redaction → quota → envelope이다.
- 정책은 지금 코드에 있다. tenant별 정책(D04 §03 "고객이 본문 수집을 켜면")은 제어 DB 정책 snapshot(D02 §02)과 함께 확장한다.

## Rollback

정책 목록을 바꾸거나 `Version`을 되돌린다. redaction 단계 자체를 건너뛰는 rollback은 허용하지 않는다(변경 불가 계약 3).

## 재검토 조건

- 오탐으로 조사 기능이 손상된다는 pilot 피드백이 있을 때.
- RUM·replay(F19·F21)에서 세션·사용자 식별이 필요할 때 (HMAC tokenization).
- 새 PII 범주나 우회 사례가 발견될 때. fixture에 추가하고 이 ADR을 갱신한다.

## 증거

- `internal/telemetry/redact` 테스트:
  - PII fixture(trace·log·metric) 회귀와 catalog 기대 누락 검사
  - `spec-reviewer`가 실제로 확인한 우회 입력 전부: camelCase·구분자 key, `db.query.parameter.*`, 인코딩 조합·잘못된 `%`·3중 인코딩, JSON·따옴표 secret, Authorization scheme, 숫자가 앞에 붙은 카드, URL 파싱 실패 userinfo, 잘린·escape된 SQL, port·목록 IP, 자유 텍스트 IP
  - 오탐 고정: epoch ms, "basic auth", Luhn 불만족 숫자열, 시각, 사설 IP
  - 문자열이 아닌 특수 key 값 처리, link 속성
  - 멱등성
  - 실패 주입: span·log·metric·scope·resource
  - 32KiB benchmark
