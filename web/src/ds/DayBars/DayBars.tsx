export interface DayBarsProps {
  values: number[];
  width?: number;
  height?: number;
  max?: number;
  format?(v: number): string;
  labels?: [number, string][];
  color?: string;
  ariaLabel?: string;
}

/**
 * A single-series bar chart for a quantity per day, such as spend.
 *
 * - One colour (`--chart-lake`), so it needs no legend. Say "who" with `RankedBars` beside it rather than stacking.
 * - Bars have 2px paper gaps. There is one y-axis with a dashed hairline grid, and ink-muted mono labels.
 * - Three x labels: start, middle and Today. The exact value shows on hover.
 * - Pass a smaller `width` on phone (about 324) so labels keep their size.
 */
export function DayBars({ values = [], width = 700, height = 190, max, format = (v) => '$' + v, labels = [], color = 'var(--chart-lake)', ariaLabel = 'Per day' }: DayBarsProps) {
  const top = 8;
  const bottom = 20;
  const x0 = 36;
  const PH = height - top - bottom;
  const hi = max || Math.max(1, Math.ceil(Math.max(0, ...values) / 10) * 10);
  const step = (width - x0) / Math.max(1, values.length);
  const bw = Math.max(step - 2, 3);
  const ticks = [0, hi / 2, hi];
  return (
    <svg className="kv-daybars" viewBox={'0 0 ' + width + ' ' + height} width="100%" role="img" aria-label={ariaLabel} style={{ maxWidth: width }}>
      {ticks.map((v) => {
        const y = top + PH - (v / hi) * PH;
        return (
          <g key={v}>
            <line x1={x0} x2={width} y1={y} y2={y} className={v ? 'is-grid' : 'is-base'} />
            <text x={0} y={y + 4}>
              {format(v)}
            </text>
          </g>
        );
      })}
      {values.map((v, d) => {
        const h = (v / hi) * PH;
        return (
          <rect key={d} x={x0 + d * step + 1} y={top + PH - h} width={bw} height={h} rx={2} style={{ fill: color }}>
            <title>{format(Number(v.toFixed(2)))}</title>
          </rect>
        );
      })}
      {labels.map(([d, l]) => (
        <text key={d} x={d === values.length - 1 ? x0 + d * step + 1 + bw : x0 + d * step + 1} y={height - 4} textAnchor={d === values.length - 1 ? 'end' : 'start'}>
          {l}
        </text>
      ))}
    </svg>
  );
}
