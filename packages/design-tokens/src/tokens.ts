// Montracer 디자인 토큰 원천 (D05 §02).
// "D05"로 표시한 값은 명세 표의 값이고, "시안"으로 표시한 값은 명세가 정하지 않아
// 디자인 시안(2026-10-04)에서 정한 값이다. 시안 값은 디자인 리뷰에서 바뀔 수 있다.

export type ThemeName = 'light' | 'dark';

export interface StatusColor {
  fg: string;
  bg: string;
  border: string;
}

export interface SeriesColor {
  name: 'blue' | 'teal' | 'orange' | 'violet';
  color: string;
  /** 색만으로 구별하지 않도록 legend·선에 함께 쓰는 선 모양 (D05 §02). */
  dash: 'solid' | 'dotted' | 'dashed' | 'dash-dot';
}

export interface ThemeColors {
  canvas: string;
  surface: string;
  surfaceSubtle: string;
  textPrimary: string;
  textMuted: string;
  borderDecorative: string;
  borderSubtle: string;
  borderControl: string;
  action: string;
  actionHover: string;
  onAction: string;
  focus: string;
  selectionBg: string;
  success: StatusColor;
  warning: StatusColor;
  critical: StatusColor;
  info: StatusColor;
  neutral: StatusColor;
  /** 점·막대처럼 글자가 아닌 warning 표시. */
  warningMark: string;
  series: readonly SeriesColor[];
}

export const colors: Record<ThemeName, ThemeColors> = {
  light: {
    canvas: '#F5F7FA', // D05
    surface: '#FFFFFF', // D05
    surfaceSubtle: '#F8FAFC', // 시안: 표 header
    textPrimary: '#17212F', // D05
    textMuted: '#526174', // D05
    borderDecorative: '#D8E0EA', // D05
    borderSubtle: '#EEF1F5', // 시안: 표 행 구분선·그래프 격자
    borderControl: '#64748B', // D05
    action: '#1D4ED8', // D05
    actionHover: '#1E3A8A', // 시안
    onAction: '#FFFFFF', // 시안
    focus: '#1D4ED8', // D05
    selectionBg: '#EEF3FD', // 시안
    success: { fg: '#166534', bg: '#F0FDF4', border: '#BBE5C8' }, // fg·bg D05, border 시안
    warning: { fg: '#92400E', bg: '#FFFBEB', border: '#F3D9A4' }, // fg·bg D05, border 시안
    critical: { fg: '#B91C1C', bg: '#FEF2F2', border: '#F5C2C2' }, // fg·bg D05, border 시안
    info: { fg: '#1D4ED8', bg: '#EFF4FF', border: '#C9D8FB' }, // 시안: source·sampled 배지
    neutral: { fg: '#526174', bg: '#F5F7FA', border: '#D8E0EA' }, // 시안: stale·판정 없음
    warningMark: '#B45309', // 시안
    series: [
      { name: 'blue', color: '#1D4ED8', dash: 'solid' }, // 순서·이름 D05, hex 시안
      { name: 'teal', color: '#0F766E', dash: 'dotted' },
      { name: 'orange', color: '#C2410C', dash: 'dashed' },
      { name: 'violet', color: '#6D28D9', dash: 'dash-dot' },
    ],
  },
  dark: {
    canvas: '#0B1220', // D05
    surface: '#111C2E', // D05
    surfaceSubtle: '#0F1828', // 시안
    textPrimary: '#E8EEF7', // D05
    textMuted: '#A4B2C5', // D05
    borderDecorative: '#344257', // D05
    borderSubtle: '#1B2738', // 시안
    borderControl: '#8292A8', // D05
    action: '#93B4FF', // D05
    actionHover: '#C7D7FF', // 시안
    onAction: '#111C2E', // 시안
    focus: '#93B4FF', // D05
    selectionBg: '#1A2B4A', // 시안
    success: { fg: '#86EFAC', bg: '#14271C', border: '#2C5A3C' }, // fg·bg D05, border 시안
    warning: { fg: '#FCD34D', bg: '#30260F', border: '#6B5520' }, // fg·bg D05, border 시안
    critical: { fg: '#FCA5A5', bg: '#321B20', border: '#6B2F38' }, // fg·bg D05, border 시안
    info: { fg: '#93B4FF', bg: '#16264A', border: '#2E4A80' }, // 시안
    neutral: { fg: '#A4B2C5', bg: '#0B1220', border: '#344257' }, // 시안
    warningMark: '#FBBF24', // 시안
    series: [
      { name: 'blue', color: '#93B4FF', dash: 'solid' }, // 시안
      { name: 'teal', color: '#5EEAD4', dash: 'dotted' },
      { name: 'orange', color: '#FDBA74', dash: 'dashed' },
      { name: 'violet', color: '#C4B5FD', dash: 'dash-dot' },
    ],
  },
};

// D05 §02: Pretendard 또는 Noto Sans KR을 로컬 번들하고 system sans-serif를 fallback으로 둔다.
export const fontFamily = {
  sans: "'Pretendard', 'Noto Sans KR', system-ui, sans-serif",
  mono: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
} as const;

export interface TypeStyle {
  sizePx: number;
  lineHeightPx: number;
  weight: 400 | 500 | 600 | 700;
  tabularNums?: true;
}

// D05 §02. 12px 미만 본문은 금지한다. body·label·code의 weight 400과 code 12/18은 시안.
export const typography = {
  title: { sizePx: 24, lineHeightPx: 32, weight: 600 },
  section: { sizePx: 18, lineHeightPx: 26, weight: 600 },
  body: { sizePx: 14, lineHeightPx: 22, weight: 400 },
  label: { sizePx: 12, lineHeightPx: 18, weight: 400 },
  metric: { sizePx: 28, lineHeightPx: 36, weight: 600, tabularNums: true },
  code: { sizePx: 12, lineHeightPx: 18, weight: 400 }, // 시안
} as const satisfies Record<string, TypeStyle>;

export const MIN_FONT_SIZE_PX = 12;

// D05 §02 치수.
export const spacePx = [4, 8, 12, 16, 24, 32] as const;

export const radiusPx = {
  control: 4,
  panel: 8,
} as const;

export const layoutPx = {
  sidebar: 224,
  sidebarCompact: 64,
  topbar: 56,
  contextRow: 48,
  contentPadding: 24,
  tableRow: 40,
  tableRowDense: 32,
  touchTarget: 44,
  control: 36,
  controlTouch: 44,
  waterfallRow: 28, // D05 §06
} as const;

export const dashboardGrid = {
  columns: 24,
  gutterPx: 16,
  rowUnitPx: 24,
} as const;

// D05 §02 반응형: 768 미만 읽기 모드, 768~1023 단일 열, 1024~1439 접힌 sidebar, 1440 이상 drawer 고정.
export const breakpointPx = {
  tablet: 768,
  desktop: 1024,
  wide: 1440,
} as const;

// 그림자는 dropdown·overlay에만 쓴다 (D05 §02). 데이터 카드는 border와 여백.
export const shadow = {
  overlay: {
    light: '0 12px 32px rgba(23, 33, 47, 0.16)',
    dark: '0 12px 32px rgba(0, 0, 0, 0.5)',
  },
} as const;
