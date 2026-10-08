import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Card } from './Card';

describe('Card', () => {
  it('renders title, meta, actions and body', () => {
    render(
      <Card title="Garden advisor" meta="Gardening · 214 notes" actions={<button>Open</button>}>
        Answers the support inbox each morning.
      </Card>,
    );
    expect(screen.getByRole('heading', { name: 'Garden advisor' })).toBeInTheDocument();
    expect(screen.getByText('Gardening · 214 notes')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Open' })).toBeInTheDocument();
    expect(screen.getByText('Answers the support inbox each morning.')).toBeInTheDocument();
  });

  it('is a disclosure when collapsible and follows defaultOpen', () => {
    const { container } = render(
      <Card title="Weekly summary" collapsible defaultOpen={false}>
        Details
      </Card>,
    );
    const details = container.querySelector('details');
    expect(details).not.toBeNull();
    expect(details).not.toHaveAttribute('open');
  });

  it('opens a collapsible card by default', () => {
    const { container } = render(
      <Card title="Weekly summary" collapsible>
        Details
      </Card>,
    );
    expect(container.querySelector('details')).toHaveAttribute('open');
  });
});
