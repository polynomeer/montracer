# PS-0007: 표 안의 screen reader용 숨김 글이 375px 화면을 가로로 늘림

- 날짜: 2026-10-08 (발견) / 2026-10-08 (해결)
- 영역: web (apps/web)
- 영향: 개발 생산성·사용성 — 좁은 화면에서 페이지 전체가 가로로 스크롤된다(D05 §12 반응형, 16px 여백 규칙). 데이터·tenant 영향 없음. 심각도 P2
- 수정: PR polynomeer/montracer#45, `apps/web/src/features/services/services.css`
- 관련: ADR 0048 결정 6, ADR 0047(S08 Metrics 미등록 서비스 ID)

## 증상

375px 화면에서 Overview(S01) 페이지의 `scrollWidth`가 771px이었다. 서비스 표는 가로 스크롤 상자(`.mt-scroll-x`) 안에 있어 상자 자체는 341px로 잘 줄었는데, 페이지 전체가 옆으로 밀렸다.

## 발견 경위

커밋 전 수동 확인(mock API, 375px에서 `document.documentElement.scrollWidth` 측정)에서 발견했다. 단위·화면 시험은 jsdom이라 layout을 계산하지 않아 잡지 못했다.

## 원인

1. 표 칸에 screen reader용 숨김 글을 넣었다. Overview는 일부 집계 표시, S08 Metrics는 미등록 서비스의 전체 ID다.
2. 숨김 글 클래스 `.mt-visually-hidden`은 `position: absolute`다.
3. `position: absolute` 요소는 가장 가까운 **positioned** 조상 기준으로 배치되고, 그 조상의 `overflow`로만 잘린다.
4. `.mt-scroll-x`는 `overflow-x: auto`만 있고 positioned가 아니었다. 그래서 숨김 글은 상자에 갇히지 않고 표의 원래 x 위치(상자 밖 769px)에 놓였다.
5. 그 결과 문서의 스크롤 너비가 늘어났다. 숨김 글은 1px 크기라 눈에 보이지 않아 원인을 찾기 어렵다.

## 해결

`.mt-scroll-x`에 `position: relative`를 주었다. 숨김 글이 스크롤 상자 기준으로 배치되어 상자의 overflow에 잘린다.

- 버린 대안: `.mt-visually-hidden`을 `position: fixed`나 `left: 0`으로 바꾸기. 전역 숨김 클래스의 동작이 바뀌어 다른 화면(skip link 주변 등)에 영향이 있다. 스크롤 상자 쪽을 고치는 편이 범위가 좁다.
- 확인: 고친 뒤 375px에서 Overview·Metrics·Logs 모두 `scrollWidth` 375였다.

## 재발 방지

- 시험: `apps/web/src/features/services/scrollBox.test.ts` — `.mt-scroll-x` 규칙에 `overflow-x: auto`와 `position: relative`가 함께 있는지 CSS 원문으로 고정한다. jsdom은 layout을 계산하지 않아 넘침 자체는 잴 수 없다.
- 검사: 새 화면의 수동 확인에 375px `scrollWidth` 측정을 넣는다(ADR 0047·0048 증거와 같은 방법).

## 교훈

`overflow`로 가두려는 상자 안에 `position: absolute` 자손이 있으면, 그 상자가 positioned여야 한다. 숨김 글은 크기가 1px이라 눈으로 찾기 어려우므로 `scrollWidth`로 잰다.
