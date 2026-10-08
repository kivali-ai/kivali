import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Prose } from './Prose';

describe('Prose', () => {
  it('renders children', () => {
    render(
      <Prose>
        <p>Hello there</p>
      </Prose>,
    );
    expect(screen.getByText('Hello there')).toBeInTheDocument();
  });

  it('renders html', () => {
    render(<Prose html="<h2>Support lead</h2><p>Checks <strong>tickets</strong>.</p>" />);
    expect(screen.getByRole('heading', { name: 'Support lead' })).toBeInTheDocument();
    expect(screen.getByText('tickets').tagName).toBe('STRONG');
  });
});
