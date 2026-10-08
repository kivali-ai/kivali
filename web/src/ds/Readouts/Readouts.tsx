import { Fragment } from 'react';
import type { ReactNode } from 'react';

export interface ReadoutItem {
  n: ReactNode;
  label: string;
  href?: string;
}

export interface ReadoutsProps {
  items: ReadoutItem[];
}

/**
 * A strip of counts at the top of a view: "5 agents working · 2 blocked · 9 closed this week · $14.20 today". Each item is a link to the view behind it.
 *
 * - The number is ink at 13px and the label ink-muted mono at 11px, separated by a middle dot.
 * - No boxes and no icons: it is a line of text. Each item links to its filtered view.
 * - Items wrap whole, never mid-phrase.
 */
export function Readouts({ items = [] }: ReadoutsProps) {
  return (
    <div className="kv-readouts">
      {items.map((i, k) => (
        <Fragment key={i.label}>
          {k > 0 && (
            <span className="kv-readouts-sep" aria-hidden="true">
              ·
            </span>
          )}
          {i.href ? (
            <a className="kv-readout" href={i.href}>
              <b>{i.n}</b> {i.label}
            </a>
          ) : (
            <span className="kv-readout">
              <b>{i.n}</b> {i.label}
            </span>
          )}
        </Fragment>
      ))}
    </div>
  );
}
