import type { ReactNode } from 'react';
import { cx } from '../cx';

export interface TableColumn<Row> {
  key: string;
  label: string;
  align?: 'left' | 'right' | 'center';
  width?: number | string;
  mono?: boolean;
  render?(row: Row): ReactNode;
}

export interface TableProps<Row> {
  columns: TableColumn<Row>[];
  rows: Row[];
  rowKey?: string;
  dense?: boolean;
  className?: string;
}

/**
 * Rows and columns for data people compare: files, usage, skills, assignment items.
 *
 * - `columns` are `{key, label, align?, width?, mono?, render?}`; `rows` are objects, keyed by `rowKey` (default `id`). `mono` sets a column in the mono face for numbers, IDs and dates; right-align numbers.
 * - Headers are small mono labels; the header row sticks while scrolling; rows hover-tint and divide with hairlines. `dense` tightens rows for long lists.
 * - The table scrolls sideways inside its rounded container on narrow screens rather than squeezing columns.
 * - If there is only one meaningful column, use `ListRow` instead.
 */
export function Table<Row extends object = Record<string, unknown>>({ columns = [], rows = [], rowKey = 'id', dense = false, className }: TableProps<Row>) {
  const cell = (r: Row, key: string) => (r as Record<string, unknown>)[key];
  return (
    <div className={cx('kv-table-wrap', className)}>
      <table className={cx('kv-table', dense && 'kv-table--dense')}>
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} style={{ textAlign: c.align || 'left', width: c.width }}>
                {c.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => {
            const k = cell(r, rowKey);
            return (
              <tr key={k != null ? String(k) : i}>
                {columns.map((c) => (
                  <td key={c.key} className={c.mono ? 'is-mono' : undefined} style={{ textAlign: c.align || 'left' }}>
                    {c.render ? c.render(r) : (cell(r, c.key) as ReactNode)}
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
