import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { CodeBlock } from './CodeBlock';

describe('CodeBlock', () => {
  it('shows the title and the code', () => {
    render(<CodeBlock title="Terminal" code="kivali hire --role gardener" />);
    expect(screen.getByText('Terminal')).toBeInTheDocument();
    expect(screen.getByText('kivali hire --role gardener')).toBeInTheDocument();
  });

  it('falls back to the language, then to "code"', () => {
    const { rerender } = render(<CodeBlock language="bash" code="ls" />);
    expect(screen.getByText('bash')).toBeInTheDocument();
    rerender(<CodeBlock code="ls" />);
    expect(screen.getByText('code')).toBeInTheDocument();
  });

  it('copies the code and confirms', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    render(<CodeBlock code="ls -la" />);
    await userEvent.click(screen.getByRole('button', { name: 'Copy' }));
    expect(writeText).toHaveBeenCalledWith('ls -la');
    expect(screen.getByRole('button', { name: 'Copied' })).toBeInTheDocument();
  });
});
