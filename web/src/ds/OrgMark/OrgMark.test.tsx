import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { OrgMark } from './OrgMark';

describe('OrgMark', () => {
  it('falls back to initials', () => {
    render(<OrgMark name="Sunset Gardens" color="pine" />);
    expect(screen.getByRole('img', { name: 'Sunset Gardens' })).toHaveTextContent('SG');
  });

  it('shows the logo when there is one', () => {
    const { container } = render(<OrgMark name="Sunset Gardens" src="/logo.svg" />);
    expect(container.querySelector('img')).toHaveAttribute('src', '/logo.svg');
    expect(screen.getByRole('img', { name: 'Sunset Gardens' })).toHaveClass('has-logo');
  });
});
