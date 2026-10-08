import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useHref, useParams } from 'react-router';
import { apiGet } from '../../api/client';
import type { ApiError } from '../../api/client';
import type { Assignment as AssignmentData, AssignmentCondition, AssignmentLink, AssignmentLogEntry, PersonRef } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { BREAKPOINT_TABBAR, AcceptanceMeter, Badge, Banner, Button, Card, Icon, AssignmentRef, AssignmentState, Menu, Prose, Skeleton, Table, Text, Toast, Tooltip, mediaQuery } from '../../ds';
import type { TableColumn } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
import { shortDate } from '../../lib/time';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { asSentence } from '../../state/home';
import { useOrg } from '../../state/OrgProvider';
import { ActDialog } from './ActDialogs';
import type { ActKind } from './ActDialogs';
import { CompactState, YOU, asApiError, readiness, stateLabel, useFaces, useSlow } from './parts';
import type { Faces } from './parts';
import '../../styles/work.css';

/** Toasts leave on their own after five seconds. */
const TOAST_MS = 5000;

/** "28 Sept 14:05" in the reader's time zone; the year appears when it is not this one. */
export function formatAt(iso: string, now: number): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return '';
  const hh = String(at.getHours()).padStart(2, '0');
  const mm = String(at.getMinutes()).padStart(2, '0');
  return shortDate(at, new Date(now)) + ' ' + hh + ':' + mm;
}

function Nobody({ children }: { children: ReactNode }) {
  return (
    <Text variant="caption" tone="muted">
      {children}
    </Text>
  );
}

/** A bare `#id` reference, as the canvas draws Parts, Waits on, Holds up and Counts toward; the title shows on hover. */
function BareRef({ id, title, state, base }: { id: number; title: string; state?: AssignmentLink['state']; base: string }) {
  return (
    <Tooltip content={'#' + id + ' ' + title}>
      <span className="app-work-ref">
        <AssignmentRef id={id} {...(state ? { state: readiness(state) } : {})} href={base + '/assignments/' + id} />
      </span>
    </Tooltip>
  );
}

/** Every link, as bare refs that wrap in a row inside the fact's cell. */
function LinkRefs({ links, base }: { links: readonly AssignmentLink[]; base: string }) {
  if (links.length === 0) return <Nobody>Nothing</Nobody>;
  return (
    <span className="app-work-refs">
      {links.map((l) => (
        <BareRef key={l.id} id={l.id} title={l.title} state={l.state} base={base} />
      ))}
    </span>
  );
}

function Person({ who, faces, withState = false }: { who: PersonRef; faces: Faces; withState?: boolean }) {
  return (
    <span className="app-work-person">
      {faces.face(who, 20)}
      <span>{who.name}</span>
      {withState && <CompactState node={faces.node(who.slug)} />}
    </span>
  );
}

interface Fact {
  label: string;
  value: ReactNode;
}

/** "Opened 28 Sept · updated 14:05": the time alone when the update was today. */
function datesText(opened: string, updated: string, now: number): string {
  const at = new Date(updated);
  const today = new Date(now);
  const sameDay = at.getFullYear() === today.getFullYear() && at.getMonth() === today.getMonth() && at.getDate() === today.getDate();
  const up = sameDay ? formatAt(updated, now).split(' ').pop() : formatAt(updated, now);
  return 'Opened ' + shortDate(new Date(opened), today) + ' · updated ' + up;
}

/**
 * The eight facts. On desktop they read in the canvas order (Assignee, Opened by, Part of, Counts toward, Parts,
 * Waits on, Holds up, Dates); on phone the four that say why it cannot move lead and the rest fold.
 */
function factsOf(a: AssignmentData, base: string, faces: Faces, now: number): { all: Fact[]; main: Fact[]; more: Fact[] } {
  const f = a.facts;
  const assignee: Fact = { label: 'Assignee', value: f.assignee?.slug ? <Person who={f.assignee} faces={faces} withState /> : <Nobody>Nobody</Nobody> };
  const openedBy: Fact = { label: 'Opened by', value: <Person who={f.opened_by} faces={faces} /> };
  const partOf: Fact = {
    label: 'Part of',
    // The one titled ref among the facts: it truncates with an ellipsis inside its cell.
    value: f.part_of ? (
      <span className="app-work-ref-fit">
        <AssignmentRef id={f.part_of.id} title={f.part_of.title} state={readiness(f.part_of.state)} href={base + '/assignments/' + f.part_of.id} />
      </span>
    ) : (
      <Nobody>Top level</Nobody>
    ),
  };
  const countsToward: Fact = {
    label: 'Counts toward',
    value:
      f.counts_toward.length === 0 ? (
        <Nobody>Nothing</Nobody>
      ) : (
        <span className="app-work-counts">
          {f.counts_toward.map((c) => (
            <span key={c.id + c.condition}>
              {c.condition}, on <BareRef id={c.id} title={c.title} base={base} />
            </span>
          ))}
        </span>
      ),
  };
  const parts: Fact = { label: 'Parts', value: <LinkRefs links={a.parts} base={base} /> };
  const waitsOn: Fact = { label: 'Waits on', value: <LinkRefs links={f.waits_on} base={base} /> };
  const holdsUp: Fact = { label: 'Holds up', value: <LinkRefs links={f.holds_up} base={base} /> };
  const dates: Fact = { label: 'Dates', value: <Text variant="caption">{datesText(f.opened, f.updated, now)}</Text> };
  return {
    all: [assignee, openedBy, partOf, countsToward, parts, waitsOn, holdsUp, dates],
    main: [assignee, partOf, waitsOn, holdsUp],
    more: [openedBy, parts, countsToward, dates],
  };
}

function FactCell({ fact }: { fact: Fact }) {
  return (
    <div className="app-work-fact">
      <Text as="dt" variant="label" tone="muted">
        {fact.label}
      </Text>
      <dd className="app-work-fact-value">{fact.value}</dd>
    </div>
  );
}

const CONDITION_BADGE: Record<string, { tone: 'success' | 'cobalt' | 'neutral'; variant: 'soft' | 'outline'; label: string }> = {
  satisfied: { tone: 'success', variant: 'soft', label: 'Met' },
  claimed: { tone: 'cobalt', variant: 'soft', label: 'In progress' },
  unclaimed: { tone: 'neutral', variant: 'outline', label: 'Unclaimed' },
};

function MetBy({ c, base, now }: { c: AssignmentCondition; base: string; now: number }) {
  if (c.state === 'satisfied' && c.met_by) {
    return (
      <span className="app-work-metby">
        <AssignmentRef id={c.met_by.id} title={c.met_by.title} state="done" href={base + '/assignments/' + c.met_by.id} />
        <Text variant="caption" tone="muted">
          {formatAt(c.met_by.at, now)}
        </Text>
      </span>
    );
  }
  if (c.claimed_by.length > 0) {
    return (
      <span className="app-work-metby">
        {c.claimed_by.map((r) => (
          <AssignmentRef key={r.id} id={r.id} title={r.title} href={base + '/assignments/' + r.id} />
        ))}
        <Text variant="caption" tone="muted">
          working
        </Text>
      </span>
    );
  }
  return <Nobody>Nobody yet</Nobody>;
}

function DoneWhen({ a, base, now }: { a: AssignmentData; base: string; now: number }) {
  const columns: TableColumn<AssignmentCondition>[] = [
    { key: 'name', label: 'Condition' },
    {
      key: 'state',
      label: 'State',
      render: (c) => {
        const b = CONDITION_BADGE[c.state] ?? CONDITION_BADGE['unclaimed'];
        return b ? (
          <Badge tone={b.tone} variant={b.variant}>
            {b.label}
          </Badge>
        ) : null;
      },
    },
    { key: 'met_by', label: 'Met by', render: (c) => <MetBy c={c} base={base} now={now} /> },
  ];
  return (
    <section className="app-work-section" aria-labelledby="assignment-done-when">
      <div className="app-work-section-head">
        <Text as="h2" variant="heading" id="assignment-done-when">
          Done when
        </Text>
        <AcceptanceMeter {...a.progress} />
      </div>
      {a.conditions.length === 0 ? (
        <Text as="p" variant="body" tone="muted">
          It names no conditions.
        </Text>
      ) : (
        <Table columns={columns} rows={a.conditions} rowKey="name" />
      )}
    </section>
  );
}

/** A log line: who, what they did in words, `#id` as a link, the reason they gave, and a link to the text before an edit. */
function LogLine({ entry, base, now }: { entry: AssignmentLogEntry; base: string; now: number }) {
  const ref = entry.ref;
  const marker = ref ? '#' + ref.id : null;
  let text: ReactNode = entry.text;
  if (ref && marker) {
    const at = entry.text.indexOf(marker);
    // Link the reference where the sentence already names it; otherwise say it after.
    text =
      at >= 0 ? (
        <>
          {entry.text.slice(0, at)}
          <AssignmentRef id={ref.id} href={base + '/assignments/' + ref.id} />
          {entry.text.slice(at + marker.length)}
        </>
      ) : (
        <>
          {entry.text} <AssignmentRef id={ref.id} href={base + '/assignments/' + ref.id} />
        </>
      );
  }
  const before = entry.before_ref;
  return (
    <li className="app-work-log-row">
      <Text variant="label" tone="muted" className="app-work-log-time">
        {formatAt(entry.at, now)}
      </Text>
      <span className="app-work-log-text">
        <strong>{entry.by.name}</strong> {text}
        {entry.note && <span className="app-work-log-note"> “{entry.note}”</span>}
        {before && (
          <>
            {' '}
            {/* eslint-disable-next-line react/forbid-elements -- external: the server serves the text before the edit outside the app */}
            <a className="app-work-before" href={before.url} target="_blank" rel="noopener noreferrer">
              before
            </a>
          </>
        )}
      </span>
    </li>
  );
}

interface ToastEntry {
  id: number;
  title: string;
}

function AssignmentToast({ entry, onDone }: { entry: ToastEntry; onDone(id: number): void }) {
  const { id } = entry;
  useEffect(() => {
    const t = setTimeout(() => onDone(id), TOAST_MS);
    return () => clearTimeout(t);
  }, [id, onDone]);
  return <Toast tone="success" title={entry.title} onDismiss={() => onDone(id)} />;
}

function Loading() {
  return (
    <div className="app-work-loading" aria-busy="true" aria-label="Loading assignment">
      <Skeleton width="30%" />
      <Skeleton width="70%" height={28} />
      <Skeleton width="100%" height={72} />
      <Skeleton width="100%" height={96} />
    </div>
  );
}

export interface AssignmentProps {
  /** The clock dates read against. Tests pass a fixed one. */
  clock?: () => number;
}

/** One assignment: state and why, facts (Parts among them), description, Done when, outcome and log; one Act menu. */
export function Assignment({ clock = Date.now }: AssignmentProps) {
  const { id: idParam } = useParams();
  const id = Number(idParam);
  const { org } = useOrg();
  const faces = useFaces();
  const base = useHref('/').replace(/\/$/, '');
  const desktop = useMediaQuery(mediaQuery(BREAKPOINT_TABBAR));
  useFrameChrome({ title: 'Assignment', tabBar: false, back: { to: '/work', label: 'Work' } });

  const [data, setData] = useState<AssignmentData | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [act, setAct] = useState<{ kind: ActKind; seq: number } | null>(null);
  const [toasts, setToasts] = useState<ToastEntry[]>([]);
  const toastId = useRef(0);
  const [now, setNow] = useState(clock);

  const seq = useRef(0);
  const load = useCallback(async () => {
    const mine = ++seq.current;
    try {
      const next = await apiGet<AssignmentData>('/api/v1/assignments/' + id);
      if (mine !== seq.current) return;
      setData(next);
      setError(null);
      setNow(clock());
    } catch (err) {
      if (mine !== seq.current) return;
      setError(asApiError(err));
    }
  }, [id, clock]);

  useEffect(() => {
    setData(null);
    void load();
    return () => {
      // A response that lands after unmount, or after a move to another assignment, is dropped.
      seq.current++;
    };
  }, [load]);

  // The record follows the tracker: a new assignments_version in the org snapshot means something changed.
  const seenVersion = useRef<number | null>(null);
  useEffect(() => {
    if (!org.ready) return;
    if (seenVersion.current === null) {
      seenVersion.current = org.assignmentsVersion;
      return;
    }
    if (seenVersion.current !== org.assignmentsVersion) {
      seenVersion.current = org.assignmentsVersion;
      void load();
    }
  }, [org.ready, org.assignmentsVersion, load]);

  const slow = useSlow(data === null && !error);
  const description = useMemo(() => (data ? renderMarkdown(data.description_md) : ''), [data]);
  const dropToast = useCallback((toast: number) => setToasts((t) => t.filter((x) => x.id !== toast)), []);

  const done = (message: string) => {
    setAct(null);
    setToasts((t) => [...t, { id: ++toastId.current, title: message }]);
    void load();
  };
  const reload = () => {
    setAct(null);
    void load();
  };

  const resolution = data?.outcome?.resolution;
  const closed = data?.state === 'closed';
  const heldHere = data?.state === 'on_hold' && data.held_here;

  const actItems = data
    ? [
        { icon: 'circle-check', label: 'Close', disabled: closed, onSelect: () => setAct({ kind: 'close', seq: data.seq }) },
        heldHere
          ? { icon: 'play', label: 'Release hold', disabled: false, onSelect: () => setAct({ kind: 'release', seq: data.seq }) }
          : { icon: 'pause', label: 'Put on hold', disabled: closed, onSelect: () => setAct({ kind: 'hold', seq: data.seq }) },
        { icon: 'pencil', label: 'Edit', disabled: closed, onSelect: () => setAct({ kind: 'edit', seq: data.seq }) },
        { separator: true as const },
        { icon: 'rotate-ccw', label: 'Reopen', disabled: !closed, onSelect: () => setAct({ kind: 'reopen', seq: data.seq }) },
      ]
    : [];
  const actMenu = (
    <Menu
      trigger={
        <Button variant="secondary" icon={<Icon name="chevron-down" />}>
          Act
        </Button>
      }
      items={actItems}
    />
  );

  const facts = data ? factsOf(data, base, faces, now) : null;

  return (
    <div className="app-work-detail">
      {error && (
        <Banner
          tone="danger"
          title={asSentence(error.message)}
          action={
            <Button size="sm" variant="secondary" onClick={() => void load()}>
              Try again
            </Button>
          }
        >
          {error.who}
        </Banner>
      )}
      {data === null && !error && slow && <Loading />}
      {data && facts && (
        <>
          {/* The Act menu sits on the state line on phone and beside the title block on desktop, as the canvas draws it. */}
          <header className="app-work-detail-head">
            <div className="app-work-detail-main">
              <div className="app-work-detail-line">
                <AssignmentState state={readiness(data.state, resolution)} {...(stateLabel(data.state) ? { label: stateLabel(data.state) as string } : {})} />
                <span className="app-work-detail-ref">
                  <AssignmentRef id={data.id} />
                  {desktop && data.facts.part_of && (
                    <Text variant="label" tone="muted">
                      {' · part of #' + data.facts.part_of.id + ' ' + data.facts.part_of.title}
                    </Text>
                  )}
                </span>
                {!desktop && actMenu}
              </div>
              <Text as="h1" variant="title">
                {data.title}
              </Text>
              {data.why && (
                <Text as="p" variant="body">
                  {asSentence(data.why)}
                </Text>
              )}
            </div>
            {desktop && actMenu}
          </header>

          <dl className="app-work-facts">
            {(desktop ? facts.all : facts.main).map((f) => (
              <FactCell key={f.label} fact={f} />
            ))}
          </dl>
          {!desktop && (
            <Card collapsible defaultOpen={false} title="More details" meta="Opened by, parts, counts toward, dates">
              <dl className="app-work-facts app-work-facts-more">
                {facts.more.map((f) => (
                  <FactCell key={f.label} fact={f} />
                ))}
              </dl>
            </Card>
          )}

          <section className="app-work-section" aria-labelledby="assignment-description">
            <Text as="h2" variant="heading" id="assignment-description">
              Description
            </Text>
            {description ? (
              <Prose html={description} />
            ) : (
              <Text as="p" variant="body" tone="muted">
                No description.
              </Text>
            )}
          </section>

          <DoneWhen a={data} base={base} now={now} />

          <section className="app-work-outcome" aria-labelledby="assignment-outcome">
            <div className="app-work-section-head">
              <Text as="h2" variant="heading" id="assignment-outcome">
                Outcome
              </Text>
              {data.outcome && (
                <>
                  <Badge tone={data.outcome.resolution === 'done' ? 'success' : 'neutral'}>{data.outcome.resolution === 'done' ? 'Done' : 'Dropped'}</Badge>
                  <Text variant="label" tone="muted">
                    {formatAt(data.outcome.at, now)}
                  </Text>
                </>
              )}
            </div>
            {data.outcome ? (
              <>
                <Text as="p" variant="body">
                  {data.outcome.text}
                </Text>
                {data.outcome.unmet.length > 0 && (
                  <Text as="p" variant="caption" tone="muted">
                    Not met when it closed: {data.outcome.unmet.join(', ')}.
                  </Text>
                )}
              </>
            ) : (
              <Text as="p" variant="body" tone="muted">
                {'Not closed yet. The outcome is written when ' + (data.facts.assignee?.slug && data.facts.assignee.slug !== YOU ? data.facts.assignee.name + ' or you close it.' : 'you close it.')}
              </Text>
            )}
          </section>

          <section className="app-work-section" aria-labelledby="assignment-log">
            <Text as="h2" variant="heading" id="assignment-log">
              Log
            </Text>
            <ol className="app-work-log">
              {data.log.map((entry, i) => (
                <LogLine key={i} entry={entry} base={base} now={now} />
              ))}
            </ol>
          </section>

          {act && <ActDialog kind={act.kind} assignment={data} seq={act.seq} onClose={() => setAct(null)} onDone={done} onReload={reload} />}
        </>
      )}
      {toasts.length > 0 && (
        <div className="app-work-toasts">
          {toasts.map((t) => (
            <AssignmentToast key={t.id} entry={t} onDone={dropToast} />
          ))}
        </div>
      )}
    </div>
  );
}
