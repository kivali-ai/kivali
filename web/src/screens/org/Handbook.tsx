import { useState } from 'react';
import { apiPut } from '../../api/client';
import type { Handbook, DocSection } from '../../api/types.gen';
import { Button, Icon, ListRow, Prose, Skeleton, Text, TextField } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
import { shortDate } from '../../lib/time';
import { ErrorBanner, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';

const PATH = '/api/v1/org/handbook';

/** How many lines a section holds, not counting the blank lines that trail it. */
export function lineCount(s: DocSection): number {
  let n = s.lines.length;
  while (n > 0 && (s.lines[n - 1] ?? '').trim() === '') n--;
  return n;
}

function Fold({ section }: { section: DocSection }) {
  const [open, setOpen] = useState(false);
  const n = lineCount(section);
  return (
    <div className="app-org-fold">
      <ListRow
        title={section.title}
        trail={
          <>
            <Text variant="label" tone="muted">
              {n} {n === 1 ? 'line' : 'lines'}
            </Text>
            <Icon name={open ? 'chevron-up' : 'chevron-down'} />
          </>
        }
        expanded={open}
        onClick={() => setOpen(!open)}
      />
      {open && (
        <div className="app-org-fold-body">
          <Prose html={renderMarkdown(section.lines.join('\n'))} />
        </div>
      )}
    </div>
  );
}

export function HandbookSection({ showHead }: SectionProps) {
  const doc = useResource<Handbook>(PATH);
  const action = useAction();
  const [draft, setDraft] = useState<string | null>(null);

  if (!doc.data) {
    return (
      <>
        <SectionHead title="Handbook" show={showHead} />
        <ErrorBanner error={doc.error} />
        {!doc.error && <Skeleton height={96} />}
      </>
    );
  }
  const data = doc.data;
  const first = data.sections[0];
  const opening = first && first.title === 'Opening' ? first : undefined;
  const folds = opening ? data.sections.slice(1) : data.sections;

  const save = () =>
    action.run(async () => {
      if (draft === null) return;
      doc.set(await apiPut<Handbook>(PATH, { content: draft }));
      setDraft(null);
    });

  const foot = data.draft
    ? 'This is the starter text. Nobody has saved a handbook yet.'
    : 'Most changes arrive from your Chief of Staff as proposals in Needs you.';
  const meta = ['Applies to every agent', `${folds.length} ${folds.length === 1 ? 'section' : 'sections'}`];
  if (data.updated_at) meta.push('updated ' + shortDate(new Date(data.updated_at), new Date()));

  return (
    <>
      <SectionHead title="Handbook" show={showHead} meta={meta.join(' · ')} />
      <ErrorBanner error={action.error} onDismiss={action.dismiss} />
      {draft !== null ? (
        <div className="app-org-editor">
          <TextField multiline rows={16} label="Handbook" value={draft} onChange={(e) => setDraft(e.target.value)} hint="Markdown. Agents read the saved text on their next turn." />
          <div className="app-org-actions">
            <Button variant="primary" onClick={() => void save()} disabled={action.busy || draft === data.content}>
              Save
            </Button>
            <Button
              variant="secondary"
              onClick={() => {
                setDraft(null);
                action.dismiss();
              }}
            >
              Cancel
            </Button>
          </div>
        </div>
      ) : (
        <div className="app-org-prose">
          {opening && <Prose html={renderMarkdown(opening.lines.join('\n'))} />}
          {folds.length > 0 && (
            <div className="app-org-folds">
              {folds.map((s) => (
                <Fold key={s.title} section={s} />
              ))}
            </div>
          )}
          <div className="app-org-actions">
            <Text variant="caption" tone="muted" className="app-org-grow">
              {foot}
            </Text>
            <Button variant="secondary" size="sm" icon={<Icon name="pencil" />} onClick={() => setDraft(data.content)}>
              Edit
            </Button>
          </div>
        </div>
      )}
    </>
  );
}
