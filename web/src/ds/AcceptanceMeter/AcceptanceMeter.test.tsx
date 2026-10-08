import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AcceptanceMeter } from './AcceptanceMeter';

describe('AcceptanceMeter', () => {
  it('reads the counts in words', () => {
    render(<AcceptanceMeter satisfied={3} claimed={1} unclaimed={1} />);
    expect(screen.getByRole('img', { name: '3 of 5 met · 1 in progress · 1 unclaimed' })).toBeInTheDocument();
    expect(screen.getByText('3/5')).toBeInTheDocument();
    expect(screen.getByText('1 unclaimed')).toBeInTheDocument();
  });

  it('omits parts that are zero', () => {
    render(<AcceptanceMeter satisfied={4} claimed={0} unclaimed={0} />);
    expect(screen.getByRole('img', { name: '4 of 4 met' })).toBeInTheDocument();
  });

  it('shows the bar alone when compact, with the text as its name', () => {
    const { container } = render(<AcceptanceMeter satisfied={1} claimed={2} unclaimed={2} compact />);
    expect(container.querySelector('.kv-acc-text')).toBeNull();
    expect(screen.getByRole('img')).toHaveAttribute('title', '1 of 5 met · 2 in progress · 2 unclaimed');
  });

  it('renders nothing without items', () => {
    const { container } = render(<AcceptanceMeter satisfied={0} claimed={0} unclaimed={0} />);
    expect(container).toBeEmptyDOMElement();
  });
});
