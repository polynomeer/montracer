import { act, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StepUpStatus, stepUpActive } from './StepUp.tsx';

afterEach(() => {
  vi.useRealTimers();
});

describe('step-up 표시', () => {
  it('유효 판정은 만료 시각 기준이다', () => {
    const now = Date.parse('2026-10-20T10:00:00Z');
    expect(stepUpActive('2026-10-20T10:15:00Z', now)).toBe(true);
    expect(stepUpActive('2026-10-20T10:00:00Z', now)).toBe(false);
    expect(stepUpActive(null, now)).toBe(false);
    expect(stepUpActive('not-a-time', now)).toBe(false);
  });

  it('만료 시각이 지나면 "유효합니다"를 내리고 추가 인증 안내로 바꾼다', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-20T10:00:00Z'));
    render(
      <MemoryRouter>
        <StepUpStatus expiresAt="2026-10-20T10:15:00Z" required={false} timeZone="UTC" />
      </MemoryRouter>,
    );
    expect(screen.getByText(/까지 유효합니다/)).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(15 * 60 * 1000 + 100);
    });
    expect(screen.queryByText(/까지 유효합니다/)).toBeNull();
    expect(screen.getByText('멤버를 바꾸려면 추가 인증이 필요합니다')).toBeTruthy();
  });
});
