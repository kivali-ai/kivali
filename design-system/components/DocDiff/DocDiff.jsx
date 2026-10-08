import React from 'react';
import { Card } from '../Card/Card.jsx';

// Split markdown into sections: text before the first ## heading becomes "Opening".
export function splitSections(md = '') {
  const out = []; let cur = { title: 'Opening', lines: [] };
  md.split('\n').forEach((l) => { const m = l.match(/^##\s+(.*)/); if (m) { if (cur.lines.some((x) => x.trim()) || out.length === 0 && cur.title !== 'Opening') out.push(cur); cur = { title: m[1].trim(), lines: [] }; } else cur.lines.push(l); });
  out.push(cur); return out.filter((s) => s.title !== 'Opening' || s.lines.some((x) => x.trim()));
}
function diffLines(a = [], b = []) {
  const A = a.filter((x) => x.trim()), B = b.filter((x) => x.trim()), res = [];
  const setA = new Set(A), setB = new Set(B);
  A.forEach((l) => { if (!setB.has(l)) res.push({ k: 'del', t: l }); });
  B.forEach((l) => res.push({ k: setA.has(l) ? 'same' : 'add', t: l }));
  return res;
}
export function DiffLine({ kind = 'same', children }) {
  return <div className={'kv-diff-line kv-diff-line--' + kind}><span className="kv-diff-mark">{kind === 'add' ? '+' : kind === 'del' ? '\u2212' : ''}</span><span className="kv-diff-text">{children}</span></div>;
}
// Before and after of a markdown document, one collapsible card per section.
export function DocDiff({ before, after, openChanged = true }) {
  const B = splitSections(before || ''), A = splitSections(after || '');
  const titles = [...new Set([...B.map((s) => s.title), ...A.map((s) => s.title)])];
  return (
    <div className="kv-docdiff">
      {titles.map((t) => {
        const lines = before == null ? (A.find((s) => s.title === t) || { lines: [] }).lines.filter((x) => x.trim()).map((l) => ({ k: 'same', t: l }))
          : diffLines((B.find((s) => s.title === t) || {}).lines, (A.find((s) => s.title === t) || {}).lines);
        const added = lines.filter((l) => l.k === 'add').length, removed = lines.filter((l) => l.k === 'del').length;
        const changed = added + removed > 0;
        const meta = before == null ? lines.length + ' lines' : !changed ? 'No changes' : [added && added + ' added', removed && removed + ' removed'].filter(Boolean).join(' · ');
        return <Card key={t} collapsible defaultOpen={before == null ? t === titles[0] : changed && openChanged} title={t} meta={meta}>{lines.map((l, i) => <DiffLine key={i} kind={l.k}>{l.t.replace(/^[-*]\s+/, '')}</DiffLine>)}</Card>;
      })}
    </div>
  );
}
