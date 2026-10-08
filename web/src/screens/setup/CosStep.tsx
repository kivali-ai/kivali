import { AgentAvatar, Banner, Button, Card, Icon, Prose, Text, TextField } from '../../ds';
import type { Setup } from '../../api/types.gen';
import { CHIEF_OF_STAFF } from '../../lib/agentIdentity';
import { renderMarkdown } from '../../lib/markdown';
import { Actions, ErrorBanner, StepHead } from './parts';
import { useState } from 'react';

/** Your edits to the two starting documents; null is untouched. */
export interface Drafts {
  role: string | null;
  handbook: string | null;
}

export const NO_DRAFTS: Drafts = { role: null, handbook: null };

/**
 * The documents open with their own "# Chief of Staff" and "# Handbook". Inside a card under the step's
 * h1 those would be second and third h1s, so every heading drops below the card title (an h3).
 */
function docHtml(md: string): string {
  const tpl = document.createElement('template');
  tpl.innerHTML = renderMarkdown(md);
  for (const h of Array.from(tpl.content.querySelectorAll('h1, h2, h3, h4, h5, h6'))) {
    const level = Math.min(6, Number(h.tagName[1]) + 3);
    const next = document.createElement('h' + level);
    next.append(...Array.from(h.childNodes));
    for (const attr of Array.from(h.attributes)) next.setAttribute(attr.name, attr.value);
    h.replaceWith(next);
  }
  return tpl.innerHTML;
}

interface DocCardProps {
  title: string;
  fieldLabel: string;
  original: string;
  draft: string | null;
  onDraft(next: string | null): void;
}

function DocCard({ title, fieldLabel, original, draft, onDraft }: DocCardProps) {
  const [editing, setEditing] = useState(false);
  const edited = draft !== null && draft !== original;
  const shown = draft ?? original;
  return (
    <Card collapsible defaultOpen={false} title={title} meta={edited ? 'Edited' : 'Default · read-only'}>
      <div className="app-setup-doc">
        {editing ? (
          <>
            <TextField multiline rows={12} label={fieldLabel} value={shown} onChange={(e) => onDraft(e.target.value)} />
            <div className="app-setup-doc-actions">
              <Button size="sm" onClick={() => setEditing(false)}>
                Done editing
              </Button>
              {edited && (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    onDraft(null);
                    setEditing(false);
                  }}
                >
                  Use the default
                </Button>
              )}
            </div>
          </>
        ) : (
          <>
            <Prose html={docHtml(shown)} />
            <div className="app-setup-doc-actions">
              <Button size="sm" icon={<Icon name="pencil" />} aria-label={'Edit ' + title.toLowerCase()} onClick={() => setEditing(true)}>
                Edit
              </Button>
            </div>
          </>
        )}
      </div>
    </Card>
  );
}

export interface CosStepProps {
  setup: Setup;
  /** Whether the hire asks it to write the handbook from the project files. */
  fromFiles: boolean;
  drafts: Drafts;
  onDrafts(next: Drafts): void;
  error: unknown;
  hiring: boolean;
  /** True while Try again is re-reading whether a model is connected. */
  checking?: boolean;
  onBack(): void;
  onHire(): void;
  onRecheck(): void;
}

/**
 * Step 4: the default role and handbook, closed and read-only until Edit; one primary. With no model
 * connected it is canvas 6j instead: what is missing, who can fix it (the server's words), and Try again.
 */
export function CosStep({ setup, fromFiles, drafts, onDrafts, error, hiring, checking = false, onBack, onHire, onRecheck }: CosStepProps) {
  if (!setup.credential.ready) {
    return (
      <>
        <StepHead title="Your Chief of Staff can’t start yet" />
        <Banner tone="warning" title="No model is connected yet">
          {(setup.credential.guidance ? setup.credential.guidance + ' ' : '') + 'Everything you entered here is saved, so you can come back and try again.'}
        </Banner>
        {error != null && <ErrorBanner error={error} />}
        <Actions>
          <Button onClick={onBack}>Back</Button>
          <span className="app-setup-spacer" />
          <Button variant="primary" loading={checking} disabled={checking} onClick={onRecheck}>
            Try again
          </Button>
        </Actions>
      </>
    );
  }
  const personal = setup.kind === 'personal';
  let framing = 'It starts with a default role and handbook. Most people leave them as they are.';
  if (personal) {
    framing =
      'It starts with a default role and handbook. Most people leave them; it asks you a few questions first, then proposes the About you section for you to approve.';
  } else if (fromFiles) {
    framing =
      'It starts with a default role and handbook. Most people leave them; once it has read your files it proposes a better handbook for you to approve.';
  }
  return (
    <>
      <StepHead title="Hire your Chief of Staff">{framing}</StepHead>
      <div className="app-setup-cos">
        <AgentAvatar {...CHIEF_OF_STAFF} size={56} />
        <div className="app-setup-cos-text">
          <Text as="div" variant="heading">
            Chief of Staff
          </Text>
          <Text as="div" variant="caption" tone="muted">
            {personal ? 'Runs your team day to day · reports to you' : 'Runs the org day to day · reports to you'}
          </Text>
        </div>
      </div>
      <DocCard
        title="Role"
        fieldLabel="Role document"
        original={setup.cos.default_role_md}
        draft={drafts.role}
        onDraft={(role) => onDrafts({ ...drafts, role })}
      />
      <DocCard
        title="Handbook"
        fieldLabel="Handbook document"
        original={setup.cos.default_handbook_md}
        draft={drafts.handbook}
        onDraft={(handbook) => onDrafts({ ...drafts, handbook })}
      />
      {error != null && <ErrorBanner error={error} />}
      <Actions>
        <Button onClick={onBack}>Back</Button>
        <span className="app-setup-spacer" />
        <Button variant="primary" disabled={hiring} loading={hiring} onClick={onHire}>
          Hire Chief of Staff
        </Button>
      </Actions>
    </>
  );
}
