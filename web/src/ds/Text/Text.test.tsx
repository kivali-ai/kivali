import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Text } from './Text';

describe('Text', () => {
  it('defaults to a body span that inherits its colour', () => {
    render(<Text>Plain</Text>);
    const el = screen.getByText('Plain');
    expect(el.tagName).toBe('SPAN');
    expect(el).toHaveClass('kv-text', 'kv-text-body');
    expect(el.className).not.toMatch(/kv-text--/);
  });

  it('renders a real heading element with the variant and tone classes', () => {
    render(
      <Text as="h2" variant="title" tone="muted">
        Inbox
      </Text>,
    );
    const h = screen.getByRole('heading', { level: 2, name: 'Inbox' });
    expect(h.tagName).toBe('H2');
    expect(h).toHaveClass('kv-text-title', 'kv-text--muted');
  });

  it('passes className and native attributes through', () => {
    render(
      <Text as="label" variant="label" htmlFor="f" id="lbl" className="extra" data-x="1">
        Name
      </Text>,
    );
    const el = screen.getByText('Name');
    expect(el.tagName).toBe('LABEL');
    expect(el).toHaveAttribute('for', 'f');
    expect(el).toHaveAttribute('id', 'lbl');
    expect(el).toHaveAttribute('data-x', '1');
    expect(el).toHaveClass('kv-text-label', 'extra');
  });
});
