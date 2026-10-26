import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { STATE_BADGE, stateBadge } from '../monitors/monitorSpec.ts';
import { ConfirmDialog, EmptyState, StatusBadge } from './states.tsx';

describe('StatusBadge', () => {
  it('데이터 없음은 점선 원·점선 테두리, 판정 없음은 실선 원 — 모양으로 구별한다', () => {
    const { container } = render(
      <>
        <StatusBadge {...STATE_BADGE.NO_DATA}>NO_DATA</StatusBadge>
        <StatusBadge tone="neutral">판정 없음</StatusBadge>
      </>,
    );
    const [noData, none] = container.querySelectorAll('.mt-badge');
    expect(noData?.className).toContain('mt-badge--dashed');
    expect(noData?.querySelector('circle')?.getAttribute('stroke-dasharray')).toBe('2 2');
    expect(none?.className).not.toContain('mt-badge--dashed');
    expect(none?.querySelector('circle')?.getAttribute('stroke-dasharray')).toBeNull();
  });

  it('평가 실패는 critical 강조(바탕 없이 실선), 정상·데이터 없음으로 보이지 않는다', () => {
    expect(STATE_BADGE.EVALUATION_ERROR).toEqual({ tone: 'critical', icon: 'triangle', variant: 'emphasis' });
    const { container } = render(<StatusBadge {...STATE_BADGE.EVALUATION_ERROR}>EVALUATION_ERROR</StatusBadge>);
    expect(container.querySelector('.mt-badge')?.className).toBe('mt-badge mt-badge--critical mt-badge--emphasis');
  });

  it('멈춘 정상은 시계 아이콘의 회색, 멈춘 경보는 경보 그대로', () => {
    expect(stateBadge('OK', true)).toEqual({ tone: 'neutral', icon: 'clock', variant: 'default' });
    expect(stateBadge('ALERT', true)).toEqual(STATE_BADGE.ALERT);
    expect(stateBadge('RECOVERING', false).icon).toBe('refresh');
  });

  it('보조 문구는 구분선 뒤에 붙고, 아이콘 없음도 고를 수 있다', () => {
    const { container } = render(
      <StatusBadge tone="critical" suffix="MUTED · 16:00까지">
        ALERT
      </StatusBadge>,
    );
    expect(screen.getByText('MUTED · 16:00까지').className).toBe('mt-badge__suffix');
    const { container: c2 } = render(
      <StatusBadge tone="neutral" icon="none" variant="dashed">
        추정
      </StatusBadge>,
    );
    expect(container.querySelector('svg')).not.toBeNull();
    expect(c2.querySelector('svg')).toBeNull();
  });
});

describe('EmptyState', () => {
  it('이유·제목·설명·다음 행동·request ID를 보인다', () => {
    render(
      <EmptyState reason="filtered" title="조건에 맞는 trace가 없습니다" requestId="req-1" action={<button type="button">조건 초기화</button>}>
        조건을 줄여 보세요.
      </EmptyState>,
    );
    expect(screen.getByText('조건에 맞는 결과 없음')).toBeTruthy();
    expect(screen.getByText('조건에 맞는 trace가 없습니다')).toBeTruthy();
    expect(screen.getByText('조건을 줄여 보세요.')).toBeTruthy();
    expect(screen.getByText('request_id req-1')).toBeTruthy();
    expect(screen.getByRole('button', { name: '조건 초기화' })).toBeTruthy();
  });

  it('이유를 모르면 꼬리표 없이 제목만', () => {
    const { container } = render(<EmptyState title="위젯이 없습니다" inline />);
    expect(container.querySelector('.mt-empty__kicker')).toBeNull();
    expect(container.querySelector('.mt-empty--inline')).not.toBeNull();
  });
});

describe('ConfirmDialog', () => {
  it('사실 목록과 오류를 보이고, 확인·취소·Escape를 전달한다', async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        id="t"
        title="삭제할까요?"
        subtitle="mon_1"
        facts={[{ term: '영향', detail: '평가가 멈춥니다.' }]}
        error="이미 삭제되었습니다"
        confirmLabel="삭제"
        busy={false}
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    );
    const dialog = screen.getByRole('alertdialog', { name: '삭제할까요?' });
    expect(dialog.textContent).toContain('평가가 멈춥니다.');
    expect(screen.getByRole('alert').textContent).toBe('이미 삭제되었습니다');
    await userEvent.click(screen.getByRole('button', { name: '삭제' }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole('button', { name: '취소' }));
    screen.getByRole('button', { name: '삭제' }).focus();
    await userEvent.keyboard('{Escape}');
    expect(onCancel).toHaveBeenCalledTimes(2);
  });

  it('진행 중에는 두 버튼이 막히고 Escape로 닫히지 않는다', async () => {
    const onCancel = vi.fn();
    render(<ConfirmDialog id="t" title="교체할까요?" confirmLabel="교체" busyLabel="교체 중…" tone="primary" busy onConfirm={vi.fn()} onCancel={onCancel} />);
    const confirm = screen.getByRole('button', { name: '교체 중…' }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    expect(confirm.className).toContain('mt-button--primary');
    expect((screen.getByRole('button', { name: '취소' }) as HTMLButtonElement).disabled).toBe(true);
    screen.getByRole('alertdialog').focus();
    await userEvent.keyboard('{Escape}');
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('오류가 나면 focus를 대화로 되돌려 Esc가 다시 된다', async () => {
    const onCancel = vi.fn();
    const { rerender } = render(<ConfirmDialog id="t" title="삭제할까요?" confirmLabel="삭제" busy onConfirm={vi.fn()} onCancel={onCancel} />);
    document.body.focus();
    rerender(<ConfirmDialog id="t" title="삭제할까요?" confirmLabel="삭제" busy={false} error="실패했습니다" onConfirm={vi.fn()} onCancel={onCancel} />);
    const dialog = screen.getByRole('alertdialog');
    expect(document.activeElement).toBe(dialog);
    expect(dialog.getAttribute('aria-describedby')).toBe('t-body');
    await userEvent.keyboard('{Escape}');
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
