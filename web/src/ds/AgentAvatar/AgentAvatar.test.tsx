import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AgentAvatar, ROLE_ICONS, colorFor, identityColors, initialsFor, roleIconNames } from './AgentAvatar';

describe('AgentAvatar', () => {
  it('names itself and shows initials', () => {
    render(<AgentAvatar name="Chief of Staff" role="compass" color="iris" />);
    const avatar = screen.getByRole('img', { name: 'Chief of Staff' });
    expect(avatar).toHaveTextContent('CS');
    expect(avatar.querySelector('svg')).not.toBeNull();
  });

  it('shows the role icon from 32px up only', () => {
    const { rerender } = render(<AgentAvatar name="Garden advisor" role="sprout" size={32} />);
    expect(screen.getByRole('img').querySelector('.kv-avatar-role')).not.toBeNull();
    rerender(<AgentAvatar name="Garden advisor" role="sprout" size={24} />);
    expect(screen.getByRole('img').querySelector('.kv-avatar-role')).toBeNull();
  });

  it('takes initials and color overrides', () => {
    render(<AgentAvatar name="x" initials="Zq" color="plum" />);
    const avatar = screen.getByRole('img', { name: 'x' });
    expect(avatar).toHaveTextContent('Zq');
    expect(avatar.style.background).toContain('--identity-plum');
  });

  it('derives a stable identity color from the name', () => {
    expect(colorFor('Bookkeeper')).toBe(colorFor('Bookkeeper'));
    expect(identityColors).toContain(colorFor('Bookkeeper'));
  });

  it('derives initials from meaningful words', () => {
    expect(initialsFor('Chief of Staff')).toBe('CS');
    expect(initialsFor('Bookkeeper')).toBe('BO');
    expect(initialsFor('')).toBe('?');
  });

  it('shows initials only without an icon, or with one the set does not draw', () => {
    for (const role of [undefined, 'robot']) {
      const { container, unmount } = render(<AgentAvatar name="Bookkeeper" {...(role ? { role } : {})} size={40} />);
      const avatar = container.querySelector('.kv-avatar--agent');
      expect(avatar).toHaveTextContent('BO');
      expect(avatar?.querySelector('svg')).toBeNull();
      expect(avatar).not.toHaveClass('kv-avatar--full');
      unmount();
    }
  });

  it('exports the role icon set', () => {
    expect(roleIconNames).toEqual(Object.keys(ROLE_ICONS));
    expect(roleIconNames).toContain('user');
  });
});
