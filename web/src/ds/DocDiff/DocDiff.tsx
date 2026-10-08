import type { ReactNode } from 'react';
import { Card } from '../Card/Card';

export interface DocSection {
  title: string;
  lines: string[];
}

/** Splits markdown into sections: text before the first ## heading becomes "Opening". */
export function splitSections(md = ''): DocSection[] {
  const out: DocSection[] = [];
  let cur: DocSection = { title: 'Opening', lines: [] };
  md.split('\n').forEach((l) => {
    const m = l.match(/^##\s+(.*)/);
    if (m) {
      if (cur.lines.some((x) => x.trim()) || (out.length === 0 && cur.title !== 'Opening')) out.push(cur);
      cur = { title: (m[1] ?? '').trim(), lines: [] };
    } else cur.lines.push(l);
  });
  out.push(cur);
  return out.filter((s) => s.title !== 'Opening' || s.lines.some((x) => x.trim()));
}

type DiffKind = 'same' | 'add' | 'del';

/** Line diff by longest common subsequence: a removed line sits where it was, just before whatever replaced it. */
export function diffLines(a: string[] = [], b: string[] = []): { k: DiffKind; t: string }[] {
  const A = a.filter((x) => x.trim());
  const B = b.filter((x) => x.trim());
  // lcs[i][j] is the LCS length of A[i..] and B[j..].
  const lcs = Array.from({ length: A.length + 1 }, () => new Array<number>(B.length + 1).fill(0));
  for (let i = A.length - 1; i >= 0; i--) {
    for (let j = B.length - 1; j >= 0; j--) {
      lcs[i]![j] = A[i] === B[j] ? lcs[i + 1]![j + 1]! + 1 : Math.max(lcs[i + 1]![j]!, lcs[i]![j + 1]!);
    }
  }
  const res: { k: DiffKind; t: string }[] = [];
  let i = 0;
  let j = 0;
  while (i < A.length && j < B.length) {
    if (A[i] === B[j]) {
      res.push({ k: 'same', t: B[j]! });
      i++;
      j++;
    } else if (lcs[i + 1]![j]! >= lcs[i]![j + 1]!) res.push({ k: 'del', t: A[i++]! });
    else res.push({ k: 'add', t: B[j++]! });
  }
  while (i < A.length) res.push({ k: 'del', t: A[i++]! });
  while (j < B.length) res.push({ k: 'add', t: B[j++]! });
  return res;
}

export interface DiffLineProps {
  kind?: DiffKind;
  children?: ReactNode;
}

/** One line of a section: added (+), removed (−, struck through) or unchanged. */
export function DiffLine({ kind = 'same', children }: DiffLineProps) {
  return (
    <div className={'kv-diff-line kv-diff-line--' + kind}>
      <span className="kv-diff-mark">{kind === 'add' ? '+' : kind === 'del' ? '−' : ''}</span>
      <span className="kv-diff-text">{children}</span>
    </div>
  );
}

export interface DocDiffProps {
  before?: string | null;
  after: string;
  openChanged?: boolean;
}

/**
 * Shows a markdown document (a role, the handbook) as collapsible sections, as a before and after diff or as a plain read.
 *
 * - `<DocDiff before={current} after={proposed} />`, or `<DocDiff after={proposed} />` for a new document such as a hire.
 * - Each `##` heading becomes a card. Text before the first heading becomes an "Opening" card.
 * - Added lines sit on cobalt-soft with "+"; removed lines are struck through in ink-muted with "−". No red and no green: nothing here has failed.
 * - Changed sections open; unchanged ones are closed with "No changes" in their meta.
 * - When a proposal carries two documents (a role and an initial memory), give each its own heading block above its cards.
 * - The line diff is per section, by longest common subsequence, so removed lines sit in place.
 */
export function DocDiff({ before, after, openChanged = true }: DocDiffProps) {
  const B = splitSections(before || '');
  const A = splitSections(after || '');
  const titles = [...new Set([...B.map((s) => s.title), ...A.map((s) => s.title)])];
  return (
    <div className="kv-docdiff">
      {titles.map((t) => {
        const aSec = A.find((s) => s.title === t);
        const bSec = B.find((s) => s.title === t);
        const lines: { k: DiffKind; t: string }[] =
          before == null
            ? (aSec?.lines ?? []).filter((x) => x.trim()).map((l) => ({ k: 'same' as const, t: l }))
            : diffLines(bSec?.lines, aSec?.lines);
        const added = lines.filter((l) => l.k === 'add').length;
        const removed = lines.filter((l) => l.k === 'del').length;
        const changed = added + removed > 0;
        const meta =
          before == null
            ? lines.length + ' lines'
            : !changed
              ? 'No changes'
              : [added && added + ' added', removed && removed + ' removed'].filter(Boolean).join(' · ');
        return (
          <Card key={t} collapsible defaultOpen={before == null ? t === titles[0] : changed && openChanged} title={t} meta={meta}>
            {lines.map((l, i) => (
              <DiffLine key={i} kind={l.k}>
                {l.t.replace(/^[-*]\s+/, '')}
              </DiffLine>
            ))}
          </Card>
        );
      })}
    </div>
  );
}
