import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AssignmentRef } from './AssignmentRef';

describe('AssignmentRef', () => {
  it('shows the number', () => {
    render(<AssignmentRef id={12} />);
    expect(screen.getByText('#12')).toBeInTheDocument();
  });

  it('is a link with the full title on hover', () => {
    render(<AssignmentRef id={15} href="/work/15" state="blocked" title="Price out three mail providers" />);
    const link = screen.getByRole('link', { name: /#15/ });
    expect(link).toHaveAttribute('href', '/work/15');
    expect(link).toHaveAttribute('title', '#15 Price out three mail providers');
  });
});
