import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function EmptyState({ title, children, action, className }) {
  // The logo's stepped pair set into the brand dot grid.
  const P = 12, cols = 8, rows = 5, r = 6.5;
  const honey = [[2, 1], [5, 2]];
  const dots = [];
  for (let y = 0; y < rows; y++) for (let x = 0; x < cols; x++) {
    if (honey.some(([hx, hy]) => hx === x && hy === y)) continue;
    dots.push(<circle key={x + '-' + y} cx={6 + x * P} cy={6 + y * P} r="1.75" className="kv-empty-dot" />);
  }
  return (
    <div className={cx('kv-empty', className)}>
      <svg className="kv-empty-art" width="120" height="75" viewBox="0 0 96 60" aria-hidden="true">
        {dots}
        {honey.map(([hx, hy]) => <circle key={'h' + hx} cx={6 + hx * P} cy={6 + hy * P} r={r} className="kv-empty-honey" />)}
      </svg>
      <h3 className="kv-empty-title">{title}</h3>
      {children && <p className="kv-empty-body">{children}</p>}
      {action && <div className="kv-empty-action">{action}</div>}
    </div>
  );
}
