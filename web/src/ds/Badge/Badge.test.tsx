import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Badge } from './Badge';

describe('Badge', () => {
  it('renders its text', () => {
    render(<Badge tone="cobalt">Learned 3 things</Badge>);
    expect(screen.getByText('Learned 3 things')).toBeInTheDocument();
  });

  it('passes native attributes through', () => {
    render(<Badge title="Model" mono variant="outline">opus · high</Badge>);
    expect(screen.getByTitle('Model')).toHaveTextContent('opus · high');
  });
});
