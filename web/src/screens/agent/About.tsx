import { useMemo, useState } from 'react';
import { useSearchParams } from 'react-router';
import { apiPut } from '../../api/client';
import type { ApiError } from '../../api/client';
import type { AgentDoc, AgentDocKind } from '../../api/types.gen';
import { Banner, Button, Icon, Prose, Skeleton, Text, TextField } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
import { useAgentPage } from './AgentPage';
import { useChatDeps } from './chatDeps';
import { SecurityPane } from './SecurityPane';
import { SoftwarePane } from './SoftwarePane';
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

type AboutView = AgentDocKind | 'software' | 'security';

const SHELL_VIEWS: readonly { kind: AboutView; label: string }[] = [
  { kind: 'software', label: 'Software' },
  { kind: 'security', label: 'Security' },
];

/**
 * About: Role, Habits and Memory, one at a time, each read first with Edit swapping in the editor; Software, the
 * programs installed in the agent's shell; and Security, where the agent's commands can connect beyond the team's
 * defaults. An archived agent has no shell, so neither Software nor Security.
 *
 * The view is the address's `?view=` (role, habits, memory, software or security), so a link can open one; any
 * other value, or a view the agent does not have, is Role, which has no parameter.
 */
export function About() {
  const { slug, name, archived, detail } = useAgentPage();
  const [params, setParams] = useSearchParams();
  const views: readonly { kind: AboutView; label: string }[] = archived ? KINDS : [...KINDS, ...SHELL_VIEWS];
  const asked = params.get('view');
  const view: AboutView = views.find((k) => k.kind === asked)?.kind ?? 'role';
  const doc = KINDS.find((k) => k.kind === view);
  // Until the agent's detail says whether it is archived, a shell view may be one it does not have: wait for
  // it rather than reading the agent's shell settings and then falling back to Role.
  const waiting = !doc && detail === null;
  return (
    <div className="app-about">
      <div className="app-about-kinds" role="group" aria-label="Document">
        {views.map((k) => (
          <Button
            key={k.kind}
            size="sm"
            variant={k.kind === view ? 'secondary' : 'ghost'}
            aria-pressed={k.kind === view}
            onClick={() => setParams(k.kind === 'role' ? {} : { view: k.kind })}
          >
            {k.label}
          </Button>
        ))}
      </div>
      {doc ? (
        <DocPane key={view} slug={slug} name={name} kind={doc.kind} label={doc.label} archived={archived} />
      ) : waiting ? (
        <div className="app-about-pane" aria-busy="true">
          <Skeleton height={56} />
          <Skeleton height={120} />
        </div>
      ) : view === 'software' ? (
        <SoftwarePane slug={slug} name={name} />
      ) : (
        <SecurityPane slug={slug} name={name} />
      )}
    </div>
  );
}
