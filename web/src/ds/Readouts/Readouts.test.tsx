import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Readouts } from './Readouts';

describe('Readouts', () => {
  it('renders counts with labels and links the ones with an href', () => {
    render(
      <Readouts
        items={[
          { n: 3, label: 'blocked', href: '/work?state=blocked' },
          { n: '$248', label: 'spent this month' },
        ]}
      />,
    );
    expect(screen.getByRole('link', { name: '3 blocked' })).toHaveAttribute('href', '/work?state=blocked');
    expect(screen.getByText('spent this month')).toBeInTheDocument();
    expect(screen.getByText('$248')).toBeInTheDocument();
  });

  it('separates items with a middle dot hidden from assistive technology', () => {
    const { container } = render(<Readouts items={[{ n: 1, label: 'a' }, { n: 2, label: 'b' }]} />);
    expect(container.querySelectorAll('.kv-readouts-sep')).toHaveLength(1);
    expect(container.querySelector('.kv-readouts-sep')).toHaveAttribute('aria-hidden', 'true');
  });
});
