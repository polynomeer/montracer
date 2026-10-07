// 테마 선택 (ADR 0040). 기본은 시스템 설정(prefers-color-scheme)이고,
// 사용자가 고르면 <html data-theme>로 강제한다. 선택은 이 브라우저에만 저장하는 UI 상태다.

export type ThemePreference = 'system' | 'light' | 'dark';

const STORAGE_KEY = 'montracer.theme';

export function isThemePreference(v: unknown): v is ThemePreference {
  return v === 'system' || v === 'light' || v === 'dark';
}

export function loadThemePreference(): ThemePreference {
  try {
    const v = window.localStorage.getItem(STORAGE_KEY);
    return isThemePreference(v) ? v : 'system';
  } catch {
    return 'system';
  }
}

export function applyThemePreference(pref: ThemePreference, root: HTMLElement = document.documentElement): void {
  if (pref === 'system') {
    root.removeAttribute('data-theme');
  } else {
    root.setAttribute('data-theme', pref);
  }
  try {
    window.localStorage.setItem(STORAGE_KEY, pref);
  } catch {
    // 저장소를 쓸 수 없으면 이번 화면에만 적용한다.
  }
}
