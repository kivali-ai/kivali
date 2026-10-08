import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { PersonAvatar } from './PersonAvatar';

describe('PersonAvatar', () => {
  it('shows initials and its name', () => {
    render(<PersonAvatar name="Maya Chen" />);
    expect(screen.getByRole('img', { name: 'Maya Chen' })).toHaveTextContent('MC');
  });

  it('shows a photo when given one', () => {
    const { container } = render(<PersonAvatar name="Maya Chen" src="/me.png" />);
    expect(container.querySelector('img')).toHaveAttribute('src', '/me.png');
  });
});
