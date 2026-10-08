import { useMemo, useState } from 'react';
import { apiPut } from '../../api/client';
import type { ApiError } from '../../api/client';
import type { AgentDoc, AgentDocKind } from '../../api/types.gen';
import { Banner, Button, Icon, Prose, Skeleton, Text, TextField } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
import { useAgentPage } from './AgentPage';
import { useChatDeps } from './chatDeps';
import { agoPhrase, asApiError, listItemCount, stripProvenance, useFetched } from './tabsShared';
import '../../styles/agent-tabs.css';

const KINDS: readonly { kind: AgentDocKind; label: string }[] = [
  { kind: 'role', label: 'Role' },
  { kind: 'habits', label: 'Habits' },
  { kind: 'memory', label: 'Memory' },
];

const EMPTY_WORDS: Record<AgentDocKind, string> = {
  role: 'No role has been written yet.',
  habits: 'No habits have been written yet.',
  memory: 'Nothing remembered yet.',
};

const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const count = (n: number, one: string, many: string) => n + ' ' + (n === 1 ? one : many);

/**
 * The one-line description, in the canvas's words: "What it does · updated 26 Sept", "How it works · 6 habits ·
 * updated 2 days ago", "What it remembers · 214 notes · updated 2 days ago".
 */
export function describeDoc(doc: AgentDoc, now: number): string {
  const updated = doc.updated_at ? 'updated ' + agoPhrase(doc.updated_at, now).replace(/^on /, '') : 'not written yet';
  switch (doc.kind) {
    case 'role':
      return ['What it does', updated].join(' · ');
    case 'habits': {
      const n = listItemCount(doc.content);
      return ['How it works', ...(n > 0 ? [count(n, 'habit', 'habits')] : []), updated].join(' · ');
    }
    case 'memory':
      return ['What it remembers', count(doc.stats.notes ?? 0, 'note', 'notes'), updated].join(' · ');
  }
}

function DocPane({ slug, name, kind, label, archived }: { slug: string; name: string; kind: AgentDocKind; label: string; archived: boolean }) {
  const path = '/api/v1/agents/' + encodeURIComponent(slug) + '/docs/' + kind;
  const { data: doc, error, loading, set } = useFetched<AgentDoc>(path);
  const deps = useChatDeps();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<ApiError | null>(null);
  const html = useMemo(() => renderMarkdown(kind === 'memory' ? stripProvenance(doc?.content ?? '') : (doc?.content ?? '')), [kind, doc?.content]);

  if (loading) {
    return (
      <div className="app-about-pane" aria-busy="true">
        <Skeleton height={56} />
        <Skeleton height={160} />
      </div>
    );
  }
  if (!doc) {
    return (
      <Banner tone="danger" title={error?.message ?? 'This document could not be read.'}>
        {error?.who}
      </Banner>
    );
  }

  const startEdit = () => {
    setDraft(doc.content);
    setSaveError(null);
    setEditing(true);
  };
  const cancel = () => {
    setEditing(false);
    setSaveError(null);
  };
  const save = async () => {
    setSaving(true);
    setSaveError(null);
    try {
      set(await apiPut<AgentDoc>(path, { content: draft }));
      setEditing(false);
    } catch (err) {
      setSaveError(asApiError(err));
    } finally {
      setSaving(false);
    }
  };

  if (editing) {
    return (
      <div className="app-about-pane">
        {saveError && (
          <Banner tone="danger" title={sentence(saveError.message)}>
            {saveError.who === 'you' ? 'Add some text, then save again.' : saveError.who}
          </Banner>
        )}
        <TextField
          label={label}
          multiline
          rows={14}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          hint={count(draft.length, 'character', 'characters') + '. Applies from ' + name + '’s next turn.'}
        />
        <div className="app-about-actions">
          <Button variant="secondary" onClick={cancel} disabled={saving}>
            Cancel
          </Button>
          <Button variant="primary" loading={saving} disabled={saving} onClick={() => void save()}>
            Save
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="app-about-pane">
      {error && (
        <Banner tone="warning" title={error.message}>
          {error.who}
        </Banner>
      )}
      {/* Read first: a hairline line with the description and Edit, then the document as prose. No card: a
          Card is for a decision, and this is a page to read. */}
      <div className="app-about-desc">
        <Text variant="label" tone="muted" className="app-about-desc-text">
          {describeDoc(doc, deps.now())}
        </Text>
        {!archived && (
          <Button size="sm" variant="secondary" icon={<Icon name="pencil" />} onClick={startEdit} aria-label={'Edit ' + label.toLowerCase()}>
            Edit
          </Button>
        )}
      </div>
      {html ? (
        <Prose html={html} />
      ) : (
        <Text as="p" variant="body" tone="muted">
          {EMPTY_WORDS[kind]}
        </Text>
      )}
    </div>
  );
}

/** About: Role, Habits and Memory, one at a time, each read first with Edit swapping in the editor. */
export function About() {
  const { slug, name, archived } = useAgentPage();
  const [kind, setKind] = useState<AgentDocKind>('role');
  const current = KINDS.find((k) => k.kind === kind) ?? { kind, label: 'Role' };
  return (
    <div className="app-about">
      <div className="app-about-kinds" role="group" aria-label="Document">
        {KINDS.map((k) => (
          <Button key={k.kind} size="sm" variant={k.kind === kind ? 'secondary' : 'ghost'} aria-pressed={k.kind === kind} onClick={() => setKind(k.kind)}>
            {k.label}
          </Button>
        ))}
      </div>
      <DocPane key={kind} slug={slug} name={name} kind={current.kind} label={current.label} archived={archived} />
    </div>
  );
}
