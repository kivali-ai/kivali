import { cx } from '../cx';

export interface AcceptanceMeterProps {
  satisfied: number;
  claimed: number;
  unclaimed: number;
  compact?: boolean;
}

/**
 * Progress on an assignment's acceptance items: how many named deliverables are met, in progress, or unclaimed.
 *
 * - `satisfied` fill success, `claimed` fill cobalt (an open child is working on it), `unclaimed` are outlined in signal-ink: work nobody has noticed, the thing to catch.
 * - The text reads "3/5 met · 1 in progress · 1 unclaimed". `compact` shows the bar alone (56px) for rows, with the text as its accessible name.
 * - Assignments without acceptance items show their open-children count instead; never fake a meter from child counts.
 */
export function AcceptanceMeter({ satisfied = 0, claimed = 0, unclaimed = 0, compact = false }: AcceptanceMeterProps) {
  const total = satisfied + claimed + unclaimed;
  if (!total) return null;
  const seg = (n: number, k: string) => n > 0 && <span className={'kv-acc-' + k} style={{ flexGrow: n }} />;
  const text =
    satisfied + ' of ' + total + ' met' + (claimed ? ' · ' + claimed + ' in progress' : '') + (unclaimed ? ' · ' + unclaimed + ' unclaimed' : '');
  return (
    <span className={cx('kv-acc', compact && 'kv-acc--compact')} role="img" aria-label={text} title={compact ? text : undefined}>
      <span className="kv-acc-bar">
        {seg(satisfied, 'met')}
        {seg(claimed, 'claimed')}
        {seg(unclaimed, 'open')}
      </span>
      {!compact && (
        <span className="kv-acc-text">
          <b>
            {satisfied}/{total}
          </b>{' '}
          met
          {claimed ? <> · {claimed} in progress</> : null}
          {unclaimed ? (
            <>
              {' '}
              · <em>{unclaimed} unclaimed</em>
            </>
          ) : null}
        </span>
      )}
    </span>
  );
}
