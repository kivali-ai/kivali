import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError, apiGetBlob } from '../../api/client';
import type {
  AccessField,
  AccessForm,
  IntegrationAction,
  IntegrationAuditRow,
  IntegrationSetup,
  IntegrationState,
  IntegrationUploadRefused,
  IntegrationVersionState,
} from '../../api/types.gen';
import { Badge, Button, CodeBlock, Dialog, DialogClose, Icon, Prose, Text } from '../../ds';
import type { BadgeTone } from '../../ds';
import { markdownBlocks } from '../../lib/markdown';
import { shortDate } from '../../lib/time';
import { saveFile } from './parts';

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

/** The API path of a version's bundle, a zip of the files it was installed from. A built-in's version has none. */
export function bundleApi(name: string, version: string): string {
  return integrationApi(name) + '/versions/' + encodeURIComponent(version) + '/bundle';
}

/** Fetches a version's bundle and hands it to the browser as a download, named as the server names it. */
export async function downloadBundle(name: string, version: string): Promise<void> {
  const file = await apiGetBlob(bundleApi(name, version));
  saveFile(file.blob, file.filename ?? name + '-' + version + '.zip');
}

/** The API path that rolls back to a version: a superseded one becomes the one in use again. */
export function rollBackApi(name: string, version: string): string {
  return integrationApi(name) + '/versions/' + encodeURIComponent(version) + '/rollback';
}

const STATE: Record<IntegrationState, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Waiting for approval', tone: 'signal' },
  installed: { label: 'Installed', tone: 'neutral' },
  connected: { label: 'Connected', tone: 'success' },
  disabled: { label: 'Disabled', tone: 'neutral' },
  not_included: { label: 'Not in this version of Kivali', tone: 'neutral' },
};

/** Where an integration stands in its lifecycle, as a badge. */
export function StateBadge({ state }: { state: IntegrationState }) {
  const s = STATE[state] ?? { label: state, tone: 'neutral' as const };
  return <Badge tone={s.tone}>{s.label}</Badge>;
}

/** "1 needs you", "3 need you": a connected integration's unmet requirements; empty for none. */
export function needsLabel(n: number): string {
  if (n <= 0) return '';
  return n + (n === 1 ? ' needs you' : ' need you');
}

/** "All met", "1 not met", "3 not met": where a connected integration's requirements stand, beside their heading. */
export function requirementsSummary(unmet: number): string {
  return unmet <= 0 ? 'All met' : unmet + ' not met';
}

/** The badge of a connected integration's unmet requirements: "1 needs you"; nothing for none. */
export function NeedsBadge({ needs }: { needs: number }) {
  const label = needsLabel(needs);
  if (!label) return null;
  return <Badge tone="signal">{label}</Badge>;
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

/** One heading of an integration's actions: those an access field unlocks, or those any access gives. */
export interface ActionGroup {
  /** The access field's key; empty for the actions any access gives. */
  key: string;
  heading: string;
  actions: IntegrationAction[];
}

/**
 * The actions grouped by the access each needs, as the access form presents it: those that need nothing under
 * "Any access", then one group per access field in the form's order, under the field's title (its key when the form
 * lacks the field or its title). Empty when no action needs a field, so the list stays flat.
 */
export function actionGroups(actions: readonly IntegrationAction[], form: AccessForm | undefined): ActionGroup[] {
  if (!actions.some((a) => a.access)) return [];
  const fields = form?.fields ?? [];
  const keys = fields.map((f) => f.key);
  for (const a of actions) {
    if (a.access && !keys.includes(a.access)) keys.push(a.access);
  }
  const groups: ActionGroup[] = [];
  const any = actions.filter((a) => !a.access);
  if (any.length > 0) groups.push({ key: '', heading: 'Any access', actions: any });
  for (const key of keys) {
    const members = actions.filter((a) => a.access === key);
    if (members.length === 0) continue;
    groups.push({ key, heading: fields.find((f) => f.key === key)?.title || key, actions: members });
  }
  return groups;
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
  /** Where the block stands, in a few words beside its heading, read without opening it ("All met"). */
  summary?: ReactNode;
  /** The summary's tone: muted (the default), or danger for one that needs the owner. */
  summaryTone?: 'muted' | 'danger';
  /** One small control beside the heading. */
  action?: ReactNode;
  /** `heading` for the integration page's sections; `label` (the default) for the install screen's facts and a section's own parts. */
  variant?: 'heading' | 'label';
  /** `h2` (the default) for a page's sections; `h3` for a part of one. */
  level?: 'h2' | 'h3';
  /** A chevron at the end of the heading opens and closes it, and the action shows only while it is open. */
  collapsible?: boolean;
  /** How a collapsible block the page does not control starts: closed, unless true. */
  defaultOpen?: boolean;
  /** A collapsible block the page opens and closes itself: whether it is open, and what the chevron asks for. */
  open?: boolean;
  onOpenChange?(open: boolean): void;
}

/** One labelled block of an integration's page or install screen: its heading, then what it says. */
export function Block({ label, children, summary, summaryTone = 'muted', action, variant = 'label', level = 'h2', collapsible = false, defaultOpen = false, open, onOpenChange }: BlockProps) {
  const [inner, setInner] = useState(defaultOpen);
  const isOpen = open ?? inner;
  const shown = !collapsible || isOpen;
  const toggle = () => {
    setInner(!isOpen);
    onOpenChange?.(!isOpen);
  };
  return (
    <section className="app-int-block" aria-label={label}>
      <div className="app-int-block-head">
        <Text as={level} variant={variant} {...(variant === 'label' ? { tone: 'muted' as const } : {})} className="app-int-block-title">
          {label}
        </Text>
        {summary != null && summary !== '' && (
          <Text variant="caption" tone={summaryTone} className="app-int-block-summary">
            {summary}
          </Text>
        )}
        <span className="app-int-spacer" />
        {shown && action}
        {collapsible && (
          <Button
            variant="ghost"
            size="sm"
            iconOnly
            icon={<Icon name="chevron-down" className="app-int-block-chev" />}
            aria-label={label}
            aria-expanded={isOpen}
            className="app-int-block-toggle"
            onClick={toggle}
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

/** "Kivali registers itself with the service; there is nothing to paste." for a sign-in client Kivali registers itself. */
export function registeredClientLine(signInClient: string): string | undefined {
  return signInClient === 'registered' ? 'Kivali registers itself with the service; there is nothing to paste.' : undefined;
}

/**
 * What a setup flow may do, from the manifest (IntegrationSetup), on the install screen and the integration's page:
 * its summary, every host it may send the browser to, every field it may ask for (secrets and setup-only values
 * marked) and every value it may keep (secrets marked). The declarations are the review: Kivali holds every step to
 * them.
 */
export const KEEPS_SHOWN = 'Values not marked Secret may be shown to you and to agents.';

export function SetupFacts({ setup, integration }: { setup: IntegrationSetup; integration: string }) {
  return (
    <div className="app-int-setup">
      <Text as="p" variant="body">
        {setup.summary}
      </Text>
      <Text as="p" variant="caption" tone="muted">
        Sends your browser to
      </Text>
      {setup.hosts.length === 0 ? (
        <Text as="p" variant="caption" tone="muted">
          No pages: setup stays in Kivali.
        </Text>
      ) : (
        <ul className="app-int-facts" aria-label="Sends your browser to">
          {setup.hosts.map((h) => (
            <li key={h}>
              <Text variant="code">{h}</Text>
            </li>
          ))}
        </ul>
      )}
      <Text as="p" variant="caption" tone="muted">
        {integration + ' asks for'}
      </Text>
      {setup.fields.length === 0 ? (
        <Text as="p" variant="caption" tone="muted">
          Nothing: setup asks you for no field.
        </Text>
      ) : (
        <ul className="app-int-facts" aria-label={integration + ' asks for'}>
          {setup.fields.map((f) => (
            <li key={f.key} className="app-int-fact">
              <span className="app-int-fact-title">
                <Text variant="body">{f.title}</Text>
                {f.secret && <Badge variant="outline">Secret</Badge>}
                {f.setup_only && <Badge variant="outline">Used once, never kept</Badge>}
              </span>
              <Text variant="caption" tone="muted">
                {f.words}
              </Text>
            </li>
          ))}
        </ul>
      )}
      <Text as="p" variant="caption" tone="muted">
        Keeps
      </Text>
      {setup.keeps.length === 0 ? (
        <Text as="p" variant="caption" tone="muted">
          Nothing: setup keeps no value.
        </Text>
      ) : (
        <>
          <ul className="app-int-facts" aria-label="Keeps">
            {setup.keeps.map((k) => (
              <li key={k.key} className="app-int-fact">
                <span className="app-int-fact-title">
                  <Text variant="body">{k.words}</Text>
                  {k.secret && <Badge variant="outline">Secret</Badge>}
                </span>
                <Text variant="code">{k.key}</Text>
              </li>
            ))}
          </ul>
          <Text as="p" variant="caption" tone="muted">
            {KEEPS_SHOWN}
          </Text>
        </>
      )}
      {setup.then && (
        <Text as="p" variant="caption" tone="muted">
          {setup.then === 'browser'
            ? 'Then signs in through your browser, with the app that setup made.'
            : 'Then signs in with a code, using the app that setup made.'}
        </Text>
      )}
    </div>
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
