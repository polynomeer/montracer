// 시험의 로그인 상태 기본값(ADR 0065). 화면 시험 대부분은 로그인 꺼짐(API key 모드)이다 — setup이 session 확인을 404로 답한다.
// 로그인 흐름을 보는 시험은 passThrough를 켜고 자기 fetch 대역으로 /auth/session에 답한다.
export const sessionMock = { passThrough: false };
