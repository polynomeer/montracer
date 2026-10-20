# PS-0016: 로그인 화면 "하던 화면으로" 링크가 다른 사이트로 갈 수 있었고, 키보드 화살표 한 번에 멤버 역할이 바뀌었다

- 날짜: 2026-10-20 (발견) / 2026-10-20 (해결)
- 영역: web
- 영향: 피싱 유도(open redirect), 권한 오부여 — 심각도 P1 두 건(리뷰). 배포 전 PR에서 발견
- 수정: PR polynomeer/montracer#70
- 관련: ADR 0068 §2·§3, ADR 0065(`loginURL`), `internal/session/oidc.go` `SafeReturnTo`, D04 §01·§02

## 증상

1. **open redirect**
   - `/login?error=step_up_rejected&return_to=%2F%5Cevil.example`을 열면 "하던 화면으로 돌아가기" 링크가 `href="/\evil.example"`로 그려졌다.
   - 브라우저는 `\`를 `/`로 읽으므로 이 링크는 `//evil.example`(다른 사이트)로 간다.
   - `/%09/evil.example`(tab)도 같다. URL parser가 tab·줄바꿈을 지운다.
   - 진짜 Montracer 로그인 화면에 피싱 사이트로 가는 링크가 생긴다.
2. **키보드 역할 변경**
   - Team 화면의 역할 선택 상자는 `change`가 일어나는 즉시 PATCH를 보냈다.
   - Windows Chrome·Edge는 닫힌 `<select>`에 focus가 있을 때 화살표 키만 눌러도 `change`를 낸다.
   - 표를 Tab으로 지나던 키보드 사용자가 ↓를 한 번 누르면 바로 역할이 바뀐다.
   - 순서가 viewer → developer → operator → tenant_admin이라, Operator 행에서 ↓ 한 번이 Tenant Admin 부여다(step-up이 유효하면 실제로 반영).

## 발견 경위

PR #70 spec-reviewer. 화면 시험은 정상 경로만 보았다.
- open redirect: `//` 하나만 거르는 경우만 시험했다.
- 역할 변경: 마우스 선택만 시험했다(user-event `selectOptions`).

## 원인

1. 화면의 `return_to` 검사(`startsWith('/') && !startsWith('//')`)가 서버 `SafeReturnTo`보다 약했다.
   - 서버는 역슬래시·제어 문자·길이를 거른다. 화면은 같은 규칙을 옮기지 않았다.
   - 서버를 거치지 않고 화면이 직접 링크를 만드는 곳(로그인 화면의 돌아가기)에서 차이가 드러났다.
2. "역할 변경은 되돌리기 쉬우니 바로 반영"(ADR 0068 후보 A)을 정할 때 키보드 입력 모델을 보지 않았다. `change`를 사용자 확정으로 다뤘다.

## 해결

1. `apps/web/src/api/authState.ts` `safeReturnTo`
   - 서버와 같은 규칙이다: `/` 시작, `//` 아님, `\`·제어 문자(<0x20, 0x7f) 없음, 2,048자 이하.
   - 로그인 화면 링크와 `loginURL`이 함께 쓴다.
2. 역할 선택은 "보류"만 하고 **적용** 버튼을 눌러야 보낸다(모든 행).
   - 확인 문장으로 결과를 알린다: 자기 역할은 권한 상실, Tenant Admin은 그 권한.
   - focus는 확인이 열리면 적용 버튼으로 간다. 닫히면(적용·취소) 선택 상자로 돌아간다.
   - 자기 역할이 바뀌면 session을 다시 읽는다(전체 다시 불러오기).

버린 대안
- **Tenant Admin으로 올릴 때만 확인**: Operator로 내리기 등 다른 의도치 않은 변경을 남긴다.
- **`onBlur`에 반영**: Tab 이동만으로 반영된다.

## 재발 방지

- 시험: `apps/web/src/app/InvitePage.test.tsx` "외부 주소(…)로는 돌아가기 링크를 만들지 않는다"
  - `//`, `/\`, tab, `\\`, 절대 URL, 줄바꿈 여섯 경우
  - SSO 버튼의 `return_to`도 `/`
- 시험: `apps/web/src/features/team/Team.test.tsx` "키보드로 역할을 훑어도 보내지 않고…"
  - 선택 상자에서 ↓↓를 눌러도 PATCH가 없다
  - 확인·취소의 focus
  - 자기 역할 성공 뒤 다시 불러오기
- 규칙: 화면이 서버 검증을 흉내 내야 하면(서버를 거치지 않는 링크) 서버 함수와 같은 규칙과 시험 사례를 함께 둔다. 차이가 나면 화면이 약한 쪽이다.

## 교훈

- 권한을 바꾸는 입력은 선택만으로 반영하지 않는다. 키보드·보조 기술의 이벤트 모델에서는 "고르기"와 "확정"이 마우스처럼 갈리지 않는다.
- URL 검사는 접두어 비교로 끝나지 않는다. 브라우저 URL parser의 정규화(역슬래시, 공백 제거)까지가 위협 모델이다.
