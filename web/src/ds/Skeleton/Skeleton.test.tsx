import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Skeleton } from './Skeleton';

describe('Skeleton', () => {
  it('is hidden from assistive technology and sized as asked', () => {
    const { container } = render(<Skeleton width={180} height={20} />);
    const el = container.firstElementChild as HTMLElement;
    expect(el).toHaveAttribute('aria-hidden', 'true');
    expect(el.style.width).toBe('180px');
    expect(el.style.height).toBe('20px');
  });

  it('rounds fully when round', () => {
    const { container } = render(<Skeleton round />);
    expect((container.firstElementChild as HTMLElement).style.borderRadius).toBe('9999px');
  });
});
