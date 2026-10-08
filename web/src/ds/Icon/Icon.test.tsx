import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ICONS, Icon, iconNames } from './Icon';

describe('Icon', () => {
  it('is decorative by default', () => {
    const { container } = render(<Icon name="inbox" />);
    const svg = container.querySelector('svg');
    expect(svg).toHaveAttribute('aria-hidden', 'true');
    expect(svg).toHaveAttribute('width', '16');
  });

  it('names itself when given a label', () => {
    render(<Icon name="inbox" size={24} label="Inbox" />);
    expect(screen.getByRole('img', { name: 'Inbox' })).toHaveAttribute('width', '24');
  });

  it('renders nothing for an unknown name', () => {
    const { container } = render(<Icon name="no-such-icon" />);
    expect(container).toBeEmptyDOMElement();
  });

  it('lists every icon in the map', () => {
    expect(iconNames).toEqual(Object.keys(ICONS));
    expect(iconNames.length).toBeGreaterThan(50);
  });
});
