import { createRef } from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Icon } from '../Icon/Icon';
import { Button } from './Button';

describe('Button', () => {
  it('renders its label and fires onClick', async () => {
    const onClick = vi.fn();
    render(<Button variant="primary" onClick={onClick}>Hire agent</Button>);
    await userEvent.click(screen.getByRole('button', { name: 'Hire agent' }));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('marks itself busy while loading and keeps the label', () => {
    render(<Button loading>Saving</Button>);
    const btn = screen.getByRole('button', { name: 'Saving' });
    expect(btn).toHaveAttribute('aria-busy', 'true');
    expect(btn).toHaveClass('is-loading');
  });

  it('hides the label for iconOnly and uses aria-label', () => {
    render(<Button iconOnly icon={<Icon name="ellipsis" />} aria-label="More">Hidden</Button>);
    expect(screen.getByRole('button', { name: 'More' })).toBeInTheDocument();
    expect(screen.queryByText('Hidden')).not.toBeInTheDocument();
  });

  it('is disabled when asked and forwards its ref', () => {
    const ref = createRef<HTMLButtonElement>();
    render(<Button ref={ref} disabled>Off</Button>);
    expect(screen.getByRole('button', { name: 'Off' })).toBeDisabled();
    expect(ref.current).toBe(screen.getByRole('button'));
  });
});
