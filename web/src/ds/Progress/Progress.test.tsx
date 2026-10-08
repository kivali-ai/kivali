import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Progress } from './Progress';

describe('Progress', () => {
  it('exposes the value and label', () => {
    render(<Progress label="Uploading 3 files" value={62} />);
    const bar = screen.getByRole('progressbar', { name: 'Uploading 3 files' });
    expect(bar).toHaveAttribute('aria-valuenow', '62');
    expect(bar).toHaveAttribute('aria-valuemax', '100');
    expect(screen.getByText('62%')).toBeInTheDocument();
  });

  it('names the bar without a visible label, and carries aria-valuetext', () => {
    render(<Progress aria-label="Setup" aria-valuetext="Step 2 of 4" value={2} max={4} />);
    const bar = screen.getByRole('progressbar', { name: 'Setup' });
    expect(bar).toHaveAttribute('aria-valuetext', 'Step 2 of 4');
    expect(bar).toHaveAttribute('aria-valuenow', '2');
    expect(screen.queryByText('50%')).toBeNull();
  });

  it('lets aria-label override the visible label as the accessible name', () => {
    render(<Progress label="Uploading" aria-label="Upload progress" value={10} />);
    expect(screen.getByRole('progressbar', { name: 'Upload progress' })).toBeInTheDocument();
    expect(screen.getByText('Uploading')).toBeInTheDocument();
  });

  it('clamps to the range and honours max', () => {
    render(<Progress label="Setup" value={3} max={4} />);
    expect(screen.getByText('75%')).toBeInTheDocument();
  });
});
