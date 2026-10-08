import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function AcceptanceMeter({ satisfied = 0, claimed = 0, unclaimed = 0, compact = false }) {
  const total = satisfied + claimed + unclaimed;
  if (!total) return null;
  const seg = (n, k) => n > 0 && <span className={'kv-acc-' + k} style={{ flexGrow: n }} />;
  const text = satisfied + ' of ' + total + ' met' + (claimed ? ' · ' + claimed + ' in progress' : '') + (unclaimed ? ' · ' + unclaimed + ' unclaimed' : '');
  return (
    <span className={cx('kv-acc', compact && 'kv-acc--compact')} role="img" aria-label={text} title={compact ? text : undefined}>
      <span className="kv-acc-bar">{seg(satisfied, 'met')}{seg(claimed, 'claimed')}{seg(unclaimed, 'open')}</span>
      {!compact && <span className="kv-acc-text"><b>{satisfied}/{total}</b> met{claimed ? <> · {claimed} in progress</> : null}{unclaimed ? <> · <em>{unclaimed} unclaimed</em></> : null}</span>}
    </span>
  );
}
