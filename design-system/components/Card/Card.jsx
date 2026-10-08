import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
const cx = (...c) => c.filter(Boolean).join(' ');

export function Card({ title, meta, actions, collapsible = false, defaultOpen = true, tone, className, style, children }) {
  const head = (title || meta || actions) && (
    <div className="kv-card-head">
      <div className="kv-card-titles">
        {title && <h3 className="kv-card-title">{title}</h3>}
        {meta && <div className="kv-card-meta">{meta}</div>}
      </div>
      {actions && <div className="kv-card-actions">{actions}</div>}
      {collapsible && <span className="kv-card-chev"><Icon name="chevron-down" /></span>}
    </div>
  );
  if (collapsible) {
    return (
      <details className={cx('kv-card', 'kv-card--collapsible', tone && 'kv-card--' + tone, className)} style={style} open={defaultOpen}>
        <summary>{head}</summary>
        <div className="kv-card-body">{children}</div>
      </details>
    );
  }
  return <section className={cx('kv-card', tone && 'kv-card--' + tone, className)} style={style}>{head}{children && <div className="kv-card-body">{children}</div>}</section>;
}
