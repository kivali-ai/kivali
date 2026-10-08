import React from 'react';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Tabs({ tabs = [], value, defaultValue, onValueChange, className }) {
  const [inner, setInner] = React.useState(defaultValue || (tabs[0] && tabs[0].value));
  const cur = value !== undefined ? value : inner;
  const set = (v) => { if (value === undefined) setInner(v); onValueChange && onValueChange(v); };
  const active = tabs.find((t) => t.value === cur);
  return (
    <div className={cx('kv-tabs', className)}>
      <div className="kv-tabs-list" role="tablist">
        {tabs.map((t) => (
          <button key={t.value} role="tab" aria-selected={t.value === cur} data-state={t.value === cur ? 'active' : 'inactive'} className="kv-tab" onClick={() => set(t.value)}>
            {t.label}{t.count != null && <span className="kv-tab-count">{t.count}</span>}
          </button>
        ))}
      </div>
      {active && active.content != null && <div role="tabpanel" className="kv-tabs-panel">{active.content}</div>}
    </div>
  );
}
