import { render, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Table } from './Table';

const columns = [
  { key: 'name', label: 'Agent' },
  { key: 'runs', label: 'Runs', align: 'right' as const, mono: true },
  { key: 'state', label: 'State', render: (r: { state: string }) => <em>{r.state}</em> },
];
const rows = [
  { id: 1, name: 'Bookkeeper', runs: 42, state: 'idle' },
  { id: 2, name: 'Garden advisor', runs: 7, state: 'running' },
];

describe('Table', () => {
  it('renders headers and rows', () => {
    render(<Table columns={columns} rows={rows} />);
    expect(screen.getAllByRole('columnheader').map((h) => h.textContent)).toEqual(['Agent', 'Runs', 'State']);
    const body = screen.getAllByRole('row').slice(1);
    expect(body).toHaveLength(2);
    expect(within(body[0] as HTMLElement).getByText('Bookkeeper')).toBeInTheDocument();
    expect(within(body[1] as HTMLElement).getByText('7')).toBeInTheDocument();
  });

  it('uses render for custom cells', () => {
    render(<Table columns={columns} rows={rows} />);
    expect(screen.getByText('running').tagName).toBe('EM');
  });

  it('marks mono columns and dense tables', () => {
    const { container } = render(<Table columns={columns} rows={rows} dense />);
    expect(container.querySelector('table')).toHaveClass('kv-table--dense');
    expect(container.querySelectorAll('td.is-mono')).toHaveLength(2);
  });
});
