// browser session의 화면 쪽 상태 (ADR 0065). 서버 데이터가 아니라 인증 부속 값이라 module 상태로 둔다(저장소에 넣지 않는다).
// - CSRF token: GET /api/v1/auth/session이 준 값. 변경 요청(GET·HEAD 밖)에 X-CSRF-Token으로 싣는다(ADR 0064 §4).
//   localStorage·cookie에 두지 않는다(XSS로 읽혀도 같은 탭 수명뿐이게).
// - 미인증 처리: session으로 쓰는 중 API가 401을 주면(만료·폐기) 복귀 경로를 담아 로그인으로 보낸다(D05 §04 "401은 복귀 URL을 보존한 로그인").

let csrfToken: string | null = null;
let onUnauthenticated: (() => void) | null = null;

export function setCSRFToken(token: string | null): void {
  csrfToken = token;
}

/** 변경 요청에 붙일 header. session이 없으면(API key proxy 모드) 비어 있다. */
export function csrfHeader(method: string): Record<string, string> {
  if (csrfToken === null || method === 'GET' || method === 'HEAD') return {};
  return { 'X-CSRF-Token': csrfToken };
}

/** session 모드일 때만 등록한다. 등록이 없으면 401은 화면의 오류 표시로 끝난다. */
export function setUnauthenticatedHandler(fn: (() => void) | null): void {
  onUnauthenticated = fn;
}

export function notifyUnauthenticated(): void {
  onUnauthenticated?.();
}

/** 페이지 밖 이동(로그인·IdP). 시험이 바꿔 끼운다. */
export const external = {
  assign(url: string): void {
    window.location.assign(url);
  },
};

/** 로그인 시작 URL. return_to는 같은 사이트 경로다(서버도 거른다). */
export function loginURL(returnTo: string): string {
  const safe = returnTo.startsWith('/') && !returnTo.startsWith('//') ? returnTo : '/';
  return `/api/v1/auth/login?return_to=${encodeURIComponent(safe)}`;
}
