import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Table({ columns = [], rows = [], rowKey = 'id', dense = false, className }) {
  return (
    <div className={cx('kv-table-wrap', className)}>
      <table className={cx('kv-table', dense && 'kv-table--dense')}>
        <thead><tr>{columns.map((c) => <th key={c.key} style={{ textAlign: c.align || 'left', width: c.width }}>{c.label}</th>)}</tr></thead>
        <tbody>{rows.map((r, i) => (
          <tr key={r[rowKey] != null ? r[rowKey] : i}>{columns.map((c) => <td key={c.key} className={c.mono ? 'is-mono' : undefined} style={{ textAlign: c.align || 'left' }}>{c.render ? c.render(r) : r[c.key]}</td>)}</tr>
        ))}</tbody>
      </table>
    </div>
  );
}
