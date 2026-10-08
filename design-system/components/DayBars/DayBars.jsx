import React from 'react';

// When: one series, one bar per day. Follows the chart rules (2px paper gaps, one y-axis, ink-muted labels).
export function DayBars({ values = [], width = 700, height = 190, max, format = (v) => '$' + v, labels = [], color = 'var(--chart-lake)', ariaLabel = 'Per day' }) {
  const top = 8, bottom = 20, x0 = 36, PH = height - top - bottom;
  const hi = max || Math.max(1, Math.ceil(Math.max(...values) / 10) * 10);
  const step = (width - x0) / Math.max(1, values.length), bw = Math.max(step - 2, 3);
  const ticks = [0, hi / 2, hi];
  return (
    <svg className="kv-daybars" viewBox={'0 0 ' + width + ' ' + height} width="100%" role="img" aria-label={ariaLabel} style={{ maxWidth: width }}>
      {ticks.map((v) => { const y = top + PH - (v / hi) * PH; return <g key={v}><line x1={x0} x2={width} y1={y} y2={y} className={v ? 'is-grid' : 'is-base'} /><text x={0} y={y + 4}>{format(v)}</text></g>; })}
      {values.map((v, d) => { const h = (v / hi) * PH; return <rect key={d} x={x0 + d * step + 1} y={top + PH - h} width={bw} height={h} rx={2} style={{ fill: color }}><title>{format(Number(v.toFixed ? v.toFixed(2) : v))}</title></rect>; })}
      {labels.map(([d, l]) => <text key={d} x={d === values.length - 1 ? x0 + d * step + 1 + bw : x0 + d * step + 1} y={height - 4} textAnchor={d === values.length - 1 ? 'end' : 'start'}>{l}</text>)}
    </svg>
  );
}
