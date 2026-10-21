import { useMemo } from 'react';
import type { ReactNode } from 'react';
import { ApiError } from '../../api/client';
import type { AccessField, IntegrationStatus, IntegrationUploadRefused, IntegrationVersionState } from '../../api/types.gen';
import { Badge, Prose, Text } from '../../ds';
import type { BadgeTone } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
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

const STATUS: Record<IntegrationStatus, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Pending', tone: 'signal' },
  not_connected: { label: 'Not connected', tone: 'neutral' },
  connected: { label: 'Connected', tone: 'success' },
  off: { label: 'Off', tone: 'neutral' },
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
}

/** One labelled block of an integration's page or install screen: its heading, then what it says. */
export function Block({ label, children, action, variant = 'label' }: BlockProps) {
  return (
    <section className="app-int-block" aria-label={label}>
      <div className="app-int-block-head">
        <Text as="h2" variant={variant} {...(variant === 'label' ? { tone: 'muted' as const } : {})} className="app-int-block-title">
          {label}
        </Text>
        {action}
      </div>
      {children}
    </section>
  );
}

/** Version notes: Markdown, sanitized, set as Prose. Nothing for empty notes. */
export function Markdown({ markdown }: { markdown: string }) {
  const html = useMemo(() => renderMarkdown(markdown), [markdown]);
  if (!html) return null;
  return <Prose html={html} className="app-int-notes" />;
}
