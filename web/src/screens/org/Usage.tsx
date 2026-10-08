import type { Usage as UsageData, UsageAgent, UsageTile, UsageWindow } from '../../api/types.gen';
import { Card, DayBars, RankedBars, Skeleton, Table, Text } from '../../ds';
import type { TableColumn } from '../../ds';
import { shortDate } from '../../lib/time';
import { ErrorBanner, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useResource } from './useResource';

export const USAGE_PATH = '/api/v1/org/usage';

export function money(v: number): string {
  return '$' + v.toFixed(2);
}

function count(n: number): string {
  return n.toLocaleString('en-US');
}

/** Token counts as the canvas writes them: 412k, 3.1M. */
export function compact(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1_000) return Math.round(n / 1_000) + 'k';
  return String(n);
}

function pct(part: number, whole: number): string {
  return whole > 0 ? Math.round((part / whole) * 100) + '%' : '0%';
}

/** "2026-08-30" as "30 Aug" (no year: the chart is the last 30 days). */
function dayLabel(iso: string): string {
  const [y, m, d] = iso.split('-').map(Number);
  if (!y || !m || !d) return iso;
  const at = new Date(y, m - 1, d);
  return shortDate(at, at);
}

const TILES: { key: 'today' | 'd7' | 'd30'; label: string }[] = [
  { key: 'today', label: 'Today' },
  { key: 'd7', label: '7 days' },
  { key: 'd30', label: '30 days' },
];

function caption(t: UsageTile): string {
  return `${count(t.calls)} calls · ${t.cache_hit_pct}% cache hit`;
}

/** The chart's three x labels: the first day, the middle day and Today. */
export function chartLabels(daily: { date: string }[]): [number, string][] {
  const first = daily[0];
  if (!first) return [];
  const last = daily.length - 1;
  const mid = Math.floor(last / 2);
  const out: [number, string][] = [[0, dayLabel(first.date)]];
  const middle = daily[mid];
  if (middle && mid > 0 && mid < last) out.push([mid, dayLabel(middle.date)]);
  if (last > 0) out.push([last, 'Today']);
  return out;
}

function byAgentColumns(total30: number): TableColumn<UsageAgent>[] {
  return [
    { key: 'name', label: 'Agent' },
    { key: 'd7', label: '7 days', align: 'right', mono: true, render: (r) => money(r.d7) },
    { key: 'd30', label: '30 days', align: 'right', mono: true, render: (r) => money(r.d30) },
    { key: 'share', label: 'Share', align: 'right', mono: true, render: (r) => pct(r.d30, total30) },
  ];
}

interface WindowRow extends UsageWindow {
  period: string;
}

// Cache hit is the tiles' measure: cache reads over everything sent, which tokens_in already sums.
const WINDOW_COLUMNS: TableColumn<WindowRow>[] = [
  { key: 'period', label: 'Period' },
  { key: 'calls', label: 'Calls', align: 'right', mono: true, render: (r) => count(r.calls) },
  { key: 'tokens_in', label: 'Tokens in', align: 'right', mono: true, render: (r) => compact(r.tokens_in) },
  { key: 'tokens_out', label: 'Tokens out', align: 'right', mono: true, render: (r) => compact(r.tokens_out) },
  { key: 'cache_read', label: 'Cache hit', align: 'right', mono: true, render: (r) => pct(r.cache_read, r.tokens_in) },
  { key: 'spend', label: 'Spend', align: 'right', mono: true, render: (r) => money(r.spend) },
];

/** The note under Usage when some calls had no list price. `unpriced` counts tokens, not calls. */
export function unpricedNote(tokens: number): string {
  return `${count(tokens)} ${tokens === 1 ? 'token' : 'tokens'} this month ran on a model with no list price, so spend leaves them out.`;
}

export function UsageSection({ phone, showHead }: SectionProps) {
  const { data, error } = useResource<UsageData>(USAGE_PATH);
  return (
    <>
      <SectionHead title="Usage" show={showHead} meta="All agents · priced at list rates" />
      <ErrorBanner error={error} />
      {!data && !error && <UsageSkeleton />}
      {data && <UsageBody data={data} phone={phone} />}
    </>
  );
}

function UsageSkeleton() {
  return (
    <div className="app-org-tiles" aria-hidden="true">
      {TILES.map((t) => (
        <div key={t.key} className="app-org-tile">
          <Skeleton width="40%" height={12} />
          <Skeleton width="60%" height={24} />
        </div>
      ))}
    </div>
  );
}

function UsageBody({ data, phone }: { data: UsageData; phone: boolean }) {
  const { tiles, daily, by_agent, windows } = data;
  const windowRows: WindowRow[] = [
    { period: 'Last 24 hours', ...windows.h24 },
    { period: 'Last 7 days', ...windows.d7 },
    { period: 'Last 30 days', ...windows.d30 },
  ];
  const total30 = by_agent.reduce((sum, a) => sum + a.d30, 0);
  const unpriced = windows.d30.unpriced;
  return (
    <>
      <div className="app-org-tiles">
        {TILES.map((t) => (
          <div key={t.key} className="app-org-tile">
            <Text variant="label" tone="muted">
              {t.label}
            </Text>
            <Text as="div" variant="heading" className="app-org-tile-value">
              {money(tiles[t.key].spend)}
            </Text>
            {!phone && (
              <Text variant="caption" tone="muted">
                {caption(tiles[t.key])}
              </Text>
            )}
          </div>
        ))}
      </div>

      <div className="app-org-panel">
        <div className="app-org-panel-head">
          <Text variant="body" className="app-org-panel-title">
            Spend per day
          </Text>
          <Text variant="label" tone="muted">
            last 30 days
          </Text>
        </div>
        <DayBars
          values={daily.map((d) => d.spend)}
          labels={chartLabels(daily)}
          format={(v) => '$' + v}
          ariaLabel="Spend per day, last 30 days"
          {...(phone ? { width: 324 } : {})}
        />
      </div>

      <div className="app-org-panel">
        <div className="app-org-panel-head">
          <Text variant="body" className="app-org-panel-title">
            Who spent it
          </Text>
          <Text variant="label" tone="muted">
            last 30 days
          </Text>
        </div>
        {by_agent.length === 0 ? (
          <Text variant="caption" tone="muted">
            No spend yet. Agents show up here once they have made calls.
          </Text>
        ) : (
          <RankedBars items={by_agent.map((a) => ({ label: a.name, value: a.d30 }))} top={5} />
        )}
      </div>

      <Card collapsible defaultOpen={false} title="By agent" meta={`All ${by_agent.length} · ranked by 30-day spend`}>
        <Table columns={byAgentColumns(total30)} rows={by_agent} rowKey="slug" dense />
      </Card>

      <Card collapsible defaultOpen={false} title="Calls and tokens" meta="24 hours · 7 days · 30 days">
        <Table columns={WINDOW_COLUMNS} rows={windowRows} rowKey="period" />
      </Card>
      {unpriced > 0 && (
        <Text as="p" variant="caption" tone="muted" className="app-org-plain">
          {unpricedNote(unpriced)}
        </Text>
      )}
    </>
  );
}
