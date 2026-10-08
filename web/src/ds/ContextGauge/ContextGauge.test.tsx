import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ContextCount, ContextGauge } from './ContextGauge';

describe('ContextGauge', () => {
  it('is a quiet gauge under the threshold', () => {
    render(<ContextGauge value={64} />);
    expect(screen.getByText('64%')).toBeInTheDocument();
    expect(screen.getByRole('progressbar', { name: 'Context 64% full' })).toHaveAttribute('aria-valuenow', '64');
  });

  it('becomes a signal badge past the threshold and New chat turns primary', async () => {
    const onNewChat = vi.fn();
    render(<ContextGauge value={86} onNewChat={onNewChat} />);
    expect(screen.getByText('Context 86% full')).toBeInTheDocument();
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
    const btn = screen.getByRole('button', { name: 'New chat' });
    expect(btn).toHaveClass('kv-btn--primary');
    await userEvent.click(btn);
    expect(onNewChat).toHaveBeenCalledTimes(1);
  });

  it('keeps New chat secondary under the threshold', () => {
    render(<ContextGauge value={10} onNewChat={() => {}} />);
    expect(screen.getByRole('button', { name: 'New chat' })).toHaveClass('kv-btn--secondary');
  });
});

describe('ContextCount', () => {
  it('renders nothing below the threshold', () => {
    const { container } = render(<ContextCount value={79} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('shows the percentage at or past the threshold', () => {
    render(<ContextCount value={91} />);
    expect(screen.getByText('91%')).toBeInTheDocument();
  });
});
