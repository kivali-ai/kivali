import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { DiffLine, DocDiff, splitSections } from './DocDiff';

const before = 'Opening line.\n## Priorities\n- Answer support daily\n- Renew the domain\n## Budget\n- Tools under $500';
const after =
  'Opening line.\n## Priorities\n- Answer support within a day\n- Renew the domain\n- Review the plan weekly\n## Budget\n- Tools under $500';

describe('splitSections', () => {
  it('puts text before the first heading under Opening', () => {
    expect(splitSections(before).map((s) => s.title)).toEqual(['Opening', 'Priorities', 'Budget']);
  });

  it('drops an empty Opening', () => {
    expect(splitSections('## A\nx').map((s) => s.title)).toEqual(['A']);
  });
});

describe('DocDiff', () => {
  it('summarises each section and opens only the changed one', () => {
    const { container } = render(<DocDiff before={before} after={after} />);
    expect(screen.getByText('2 added · 1 removed')).toBeInTheDocument();
    expect(screen.getAllByText('No changes')).toHaveLength(2);
    const open = Array.from(container.querySelectorAll('details')).map((d) => d.hasAttribute('open'));
    expect(open).toEqual([false, true, false]);
  });

  it('marks added and removed lines', () => {
    render(<DocDiff before={before} after={after} />);
    expect(screen.getByText('Answer support daily').closest('.kv-diff-line')).toHaveClass('kv-diff-line--del');
    expect(screen.getByText('Review the plan weekly').closest('.kv-diff-line')).toHaveClass('kv-diff-line--add');
    expect(screen.getByText('Renew the domain').closest('.kv-diff-line')).toHaveClass('kv-diff-line--same');
  });

  it('puts a removed line where it was, before what replaced it', () => {
    const { container } = render(<DocDiff before={before} after={after} />);
    const rows = Array.from(container.querySelectorAll('details')[1]!.querySelectorAll('.kv-diff-line')).map(
      (r) => r.className.replace('kv-diff-line kv-diff-line--', '') + ' ' + r.querySelector('.kv-diff-text')!.textContent,
    );
    expect(rows).toEqual([
      'del Answer support daily',
      'add Answer support within a day',
      'same Renew the domain',
      'add Review the plan weekly',
    ]);
  });

  it('keeps a removed line in the middle of a section in the middle', () => {
    render(<DocDiff before={'## S\n- a\n- b\n- c\n- d'} after={'## S\n- a\n- c\n- d'} />);
    const rows = Array.from(document.querySelectorAll('.kv-diff-line')).map((r) => r.textContent);
    expect(rows).toEqual(['a', '−b', 'c', 'd']);
  });

  it('reads a new document as plain sections with line counts', () => {
    render(<DocDiff after={after} />);
    expect(screen.getByText('3 lines')).toBeInTheDocument();
  });
});

describe('DiffLine', () => {
  it('shows the mark for its kind', () => {
    const { container } = render(
      <>
        <DiffLine kind="add">a</DiffLine>
        <DiffLine kind="del">b</DiffLine>
        <DiffLine>c</DiffLine>
      </>,
    );
    expect(Array.from(container.querySelectorAll('.kv-diff-mark')).map((m) => m.textContent)).toEqual(['+', '−', '']);
  });
});
