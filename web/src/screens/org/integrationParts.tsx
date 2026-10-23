import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError } from '../../api/client';
import type { AccessField, IntegrationAuditRow, IntegrationStatus, IntegrationUploadRefused, IntegrationVersionState } from '../../api/types.gen';
import { Badge, Button, CodeBlock, Dialog, DialogClose, Icon, Prose, Text } from '../../ds';
import type { BadgeTone } from '../../ds';
import { markdownBlocks } from '../../lib/markdown';
import { shortDate } from '../../lib/time';

/** The API's list of integrations; each one's own paths hang off it. */
export const INTEGRATIONS_API = '/api/v1/org/integrations';

export function integrationApi(name: string): string {
  return INTEGRATIONS_API + '/' + encodeURIComponent(name);
}

/** The router path of an integration's page under Org. */
export function integrationPath(name: string): string {
  return '/org/integrations/' + encodeURIComponent(name);
}

/** The router path of a version's install screen. */
export function versionPath(name: string, version: string): string {
  return integrationPath(name) + '/versions/' + encodeURIComponent(version);
}

/** The API path that rolls back to a version: a superseded one becomes the one in use again. */
export function rollBackApi(name: string, version: string): string {
  return integrationApi(name) + '/versions/' + encodeURIComponent(version) + '/rollback';
}

const STATUS: Record<IntegrationStatus, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Pending', tone: 'signal' },
  not_connected: { label: 'Not connected', tone: 'neutral' },
  connected: { label: 'Connected', tone: 'success' },
  off: { label: 'Off', tone: 'neutral' },
  not_running: { label: 'Not running', tone: 'signal' },
  not_included: { label: 'Not in this version of Kivali', tone: 'neutral' },
};

/** The one word the list shows for an integration, as a badge. */
export function StatusBadge({ status }: { status: IntegrationStatus }) {
  const s = STATUS[status] ?? { label: status, tone: 'neutral' as const };
  return <Badge tone={s.tone}>{s.label}</Badge>;
}

const VERSION_STATE: Record<IntegrationVersionState, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Needs your approval', tone: 'signal' },
  approved: { label: 'In use', tone: 'success' },
  rejected: { label: 'Rejected', tone: 'neutral' },
  superseded: { label: 'Replaced', tone: 'neutral' },
};

/** Where a version stands, as a badge. */
export function VersionBadge({ state }: { state: IntegrationVersionState }) {
  const s = VERSION_STATE[state] ?? { label: state, tone: 'neutral' as const };
  return <Badge tone={s.tone}>{s.label}</Badge>;
}

/** An action's name in words: "create_post" is "Create post". */
export function actionWords(name: string): string {
  const words = name.replace(/_/g, ' ').trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** Your roll back to another version is in the audit as the action `rollback`, with you as the agent. */
export function isRollBack(row: Pick<IntegrationAuditRow, 'agent' | 'action'>): boolean {
  return row.action === 'rollback' && row.agent.slug === 'ceo';
}

/** What the page says when access rows no longer fit the version in use; empty when none. */
export function invalidAccessLine(count: number): string {
  if (count === 0) return '';
  if (count === 1) return '1 access row no longer fits this version’s access form. Set it again.';
  return count + ' access rows no longer fit this version’s access form. Set them again.';
}

/** "1 agent", "3 agents", "No agents". */
export function agentsLabel(n: number): string {
  if (n === 0) return 'No agents';
  return n + (n === 1 ? ' agent' : ' agents');
}

/** What an access field takes, in words, for the install screen. */
export function fieldKindWords(field: AccessField, integration: string): string {
  const listed = field.choices ? ', listed by ' + integration : field.options.length > 0 ? ': ' + field.options.join(', ') : '';
  switch (field.kind) {
    case 'text':
      return 'Text';
    case 'choice':
      return 'One choice' + listed;
    case 'switch':
      return 'On or off';
    case 'list':
      return 'A list of entries';
    case 'choices':
      return 'Any number of choices' + listed;
    default:
      return field.kind;
  }
}

/** "21 Oct, 10:00": a date and a time of day, in the reader's time zone. Empty for an unreadable time. */
export function whenLabel(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const at = new Date(t);
  const time = String(at.getHours()).padStart(2, '0') + ':' + String(at.getMinutes()).padStart(2, '0');
  return shortDate(at, new Date(now)) + ', ' + time;
}

/** "812 ms", "1.4 s". */
export function durationLabel(ms: number): string {
  return ms < 1000 ? ms + ' ms' : (ms / 1000).toFixed(1) + ' s';
}

/** A refused upload's 400 names every finding; any other failure is an ordinary error. */
export function uploadRefusalOf(err: unknown): IntegrationUploadRefused | null {
  if (!(err instanceof ApiError) || err.status !== 400) return null;
  const b = err.body as Partial<IntegrationUploadRefused> | null | undefined;
  if (!b || !Array.isArray(b.findings)) return null;
  return { error: err.message, who: err.who, findings: b.findings.filter((f): f is string => typeof f === 'string') };
}

interface BlockProps {
  label: string;
  children: ReactNode;
  /** One small control beside the heading. */
  action?: ReactNode;
  /** `heading` for the integration page's sections; `label` (the default) for the install screen's facts. */
  variant?: 'heading' | 'label';
  /** Starts closed: a chevron at the end of the heading opens and closes it, and the action shows only while it is open. */
  collapsible?: boolean;
}

/** One labelled block of an integration's page or install screen: its heading, then what it says. */
export function Block({ label, children, action, variant = 'label', collapsible = false }: BlockProps) {
  const [open, setOpen] = useState(false);
  const shown = !collapsible || open;
  return (
    <section className="app-int-block" aria-label={label}>
      <div className="app-int-block-head">
        <Text as="h2" variant={variant} {...(variant === 'label' ? { tone: 'muted' as const } : {})} className="app-int-block-title">
          {label}
        </Text>
        {shown && action}
        {collapsible && (
          <Button
            variant="ghost"
            size="sm"
            iconOnly
            icon={<Icon name="chevron-down" className="app-int-block-chev" />}
            aria-label={label}
            aria-expanded={open}
            className="app-int-block-toggle"
            onClick={() => setOpen((o) => !o)}
          />
        )}
      </div>
      {shown && children}
    </section>
  );
}

interface RollBackDialogProps {
  /** The version to roll back to; the dialog is open while it is set. */
  version: string | null;
  /** The version in use, which stops. */
  current: string;
  busy: boolean;
  /** The failure of the last try, shown in the dialog so it stays open. */
  error: ReactNode;
  onConfirm(): void;
  onClose(): void;
}

/** The confirm before a roll back: which version stops, which runs in its place, and what happens to access. */
export function RollBackDialog({ version, current, busy, error, onConfirm, onClose }: RollBackDialogProps) {
  const v = version ?? '';
  return (
    <Dialog
      open={version !== null}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={'Roll back to version ' + v + '?'}
      description={
        'Version ' + current + ' stops and ' + v + ' runs in its place. Agents’ access stays as it is; rows that no longer fit ' + v + '’s access form are marked.'
      }
      footer={
        <>
          <DialogClose>
            <Button variant="secondary">Cancel</Button>
          </DialogClose>
          <Button variant="primary" onClick={onConfirm} loading={busy} disabled={busy}>
            Roll back
          </Button>
        </>
      }
    >
      {error}
    </Dialog>
  );
}

/**
 * The words of each code a browser sign-in's callback may send the page in ?sign_in_error=. The code, never words,
 * is in the address: anyone can link the owner to a page with any query, so only Kivali's own sentences are shown.
 */
const SIGN_IN_ERROR_WORDS: Record<string, string> = {
  start_again: 'The sign-in took too long or was started elsewhere. Start it again.',
};

/** The sentence for a ?sign_in_error= code; undefined for none, and for a code Kivali does not send. */
export function signInErrorWords(code: string | null): string | undefined {
  return code !== null && Object.hasOwn(SIGN_IN_ERROR_WORDS, code) ? SIGN_IN_ERROR_WORDS[code] : undefined;
}

/** What a sign-in connection asks the service for, in words; undefined when it names no scopes. */
export function scopesLine(scopes: readonly string[]): string | undefined {
  return scopes.length > 0 ? 'Asks the service for: ' + scopes.join(', ') : undefined;
}

/** The scopes line of a sign-in connection, on the install screen and the page; nothing when there are none. */
export function ScopesLine({ scopes }: { scopes: readonly string[] }) {
  const line = scopesLine(scopes);
  if (!line) return null;
  return (
    <Text as="p" variant="caption" tone="muted">
      {line}
    </Text>
  );
}

/**
 * Version notes or a manifest's setup notes: Markdown, sanitized, set as Prose, with each top-level code block a
 * CodeBlock headed by its info string, so an address or a command the notes give has a copy button. Nothing for empty
 * notes.
 */
export function Markdown({ markdown }: { markdown: string }) {
  const blocks = useMemo(() => markdownBlocks(markdown), [markdown]);
  if (blocks.length === 0) return null;
  return (
    <div className="app-int-notes">
      {blocks.map((b, i) => (b.kind === 'code' ? <CodeBlock key={i} code={b.code} {...(b.info ? { title: b.info } : {})} /> : <Prose key={i} html={b.html} className="app-int-notes-text" />))}
    </div>
  );
}
