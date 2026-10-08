import { Progress } from '../Progress/Progress';

export interface RankedBarsProps {
  items: { label: string; value: number }[];
  top?: number;
  format?(v: number): string;
  restLabel?: string;
}

/**
 * A ranked list for "who": the top five as single cobalt bars, and the rest as one quiet total.
 *
 * - One colour. The rank carries the meaning; do not give each row an identity colour.
 * - Bars scale to the largest of the top N. The catch-all row sits under a hairline in ink-muted with no bar, so it is never the heaviest mark.
 * - Keep every item reachable in a folded table beside it ("By agent").
 */
export function RankedBars({ items = [], top = 5, format = (v) => '$' + Math.round(v), restLabel = 'Everyone else' }: RankedBarsProps) {
  const sorted = [...items].sort((a, b) => b.value - a.value);
  const head = sorted.slice(0, top);
  const tail = sorted.slice(top);
  const max = Math.max(1, ...head.map((i) => i.value));
  const rest = tail.reduce((a, i) => a + i.value, 0);
  return (
    <div className="kv-ranked">
      {head.map((i) => (
        <div key={i.label} className="kv-ranked-row">
          <span className="kv-ranked-label">{i.label}</span>
          <Progress value={i.value} max={max} tone="cobalt" aria-label={`${i.label} ${format(i.value)}`} />
          <span className="kv-ranked-value">{format(i.value)}</span>
        </div>
      ))}
      {tail.length > 0 && (
        <div className="kv-ranked-row kv-ranked-row--rest">
          <span className="kv-ranked-label">
            {restLabel} · {tail.length} {tail.length === 1 ? 'agent' : 'agents'}
          </span>
          <span />
          <span className="kv-ranked-value">{format(rest)}</span>
        </div>
      )}
    </div>
  );
}
