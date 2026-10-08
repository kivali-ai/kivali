import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { NodeRow } from './NodeRow';

const node = { id: 'req/login-timeout', kind: 'requirement' as const, status: 'flagged' as const, owner: 'clay' };

describe('NodeRow', () => {
  it('shows the chip and the short fact', () => {
    render(<NodeRow node={node} short="flagged by Test runner" />);
    expect(screen.getByText('req/login-timeout')).toBeInTheDocument();
    expect(screen.getByText('flagged by Test runner')).toBeInTheDocument();
  });

  it('drops the short fact when compact', () => {
    render(<NodeRow node={node} short="flagged by Test runner" compact />);
    expect(screen.queryByText('flagged by Test runner')).not.toBeInTheDocument();
  });

  it('toggles and shows its fields when expanded', async () => {
    const onToggle = vi.fn();
    const { rerender } = render(<NodeRow node={node} onToggle={onToggle} fields={[['Path', 'requirements/login-timeout.md']]} />);
    const row = screen.getByRole('button', { expanded: false });
    expect(screen.queryByText('Path')).not.toBeInTheDocument();
    await userEvent.click(row);
    expect(onToggle).toHaveBeenCalledTimes(1);
    rerender(<NodeRow node={node} expanded onToggle={onToggle} fields={[['Path', 'requirements/login-timeout.md']]} />);
    expect(screen.getByRole('button', { expanded: true })).toBeInTheDocument();
    expect(screen.getByText('Path').tagName).toBe('DT');
    expect(screen.getByText('requirements/login-timeout.md').tagName).toBe('DD');
  });
});
