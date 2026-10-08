import { useMemo } from 'react';
import type { ReactNode } from 'react';
import type { ApiError } from '../../api/client';
import type { GraphNode, GraphNodeFields, GraphRef } from '../../api/types.gen';
import { Badge, Banner, Prose, Skeleton, Text } from '../../ds';
import { renderMarkdown } from '../../lib/markdown';
import { shortDate } from '../../lib/time';
import { asSentence } from '../../state/home';

function Mono({ text }: { text: string }) {
  return <span className="app-graph-mono">{text}</span>;
}

function refText(r: GraphRef): string {
  return r.id + (r.pin ? '@' + r.pin : '') + (r.resolved ? '' : ' (missing)');
}

function list(items: readonly string[]): ReactNode {
  return <span className="app-graph-list">{items.map((i) => (
        <Mono key={i} text={i} />
      ))}</span>;
}

function refs(items: readonly GraphRef[]): ReactNode {
  return <span className="app-graph-list">{items.map((r) => (
        <Mono key={refText(r)} text={refText(r)} />
      ))}</span>;
}

/**
 * The node's fields in reading order as [label, value] pairs. Empty fields are left out: a node shows what
 * it has, not a wall of blanks.
 */
export function fieldRows(f: GraphNodeFields): [string, ReactNode][] {
  const rows: [string, ReactNode][] = [];
  const text = (label: string, value: string) => {
    if (value) rows.push([label, value]);
  };
  text('Path', f.path);
  text('Owner', f.owner.name);
  text('Type', f.type);
  text('Kind', f.kind);
  text('Status', f.declared_status && f.declared_status !== f.status ? f.status + ' (declared ' + f.declared_status + ')' : f.status);
  text('Summary', f.summary);
  text('Condition', f.condition);
  text('Check', f.check);
  text('Source', f.source);
  text('Payload', f.payload);
  text('Rejected', f.rejected);
  if (f.about) rows.push(['About', refs([f.about])]);
  if (f.depends_on.length > 0) rows.push(['Rests on', refs(f.depends_on)]);
  if (f.supersedes.length > 0) rows.push(['Supersedes', refs(f.supersedes)]);
  if (f.superseded_by.length > 0) rows.push(['Superseded by', list(f.superseded_by)]);
  if (f.evidence.length > 0) rows.push(['Evidence', list(f.evidence)]);
  if (f.flags.length > 0) rows.push(['Flags', f.flags.join('; ')]);
  if (f.problems.length > 0) rows.push(['Problems', f.problems.join('; ')]);
  if (f.dependents.length > 0) rows.push(['Dependents', list(f.dependents)]);
  if (f.about_me.length > 0) rows.push(['About this', list(f.about_me)]);
  text('Reports to', f.reports_to);
  if (f.versions.length > 0) rows.push(['Versions', String(f.versions.length)]);
  return rows;
}

export interface NodeDetailProps {
  node: GraphNode | null;
  error: ApiError | null;
  now: number;
  /** Leave out the field list: the row above already shows it. */
  bare?: boolean;
}

/** A node's fields as a definition list, then the text of the version shown with a version chip. */
export function NodeDetail({ node, error, now, bare = false }: NodeDetailProps) {
  const body = useMemo(() => (node?.body_md ? renderMarkdown(node.body_md) : ''), [node]);
  if (error) {
    return (
      <Banner tone="danger" title={asSentence(error.message)}>
        {error.who}
      </Banner>
    );
  }
  if (!node) {
    if (bare) return null;
    return (
      <div className="app-graph-loading" aria-busy="true" aria-label="Loading node">
        <Skeleton width="60%" />
        <Skeleton width="40%" />
        <Skeleton width="80%" />
      </div>
    );
  }
  const rows = bare ? [] : fieldRows(node.fields);
  if (bare && !node.version && !body && !node.body_note) return null;
  return (
    <div className="app-graph-detail">
      {!bare && (
        <dl className="app-graph-fields">
          {rows.map(([label, value]) => (
            <div key={label} className="app-graph-field">
              <Text as="dt" variant="label" tone="muted">
                {label}
              </Text>
              <dd className="app-graph-value">{value}</dd>
            </div>
          ))}
        </dl>
      )}
      {(node.version || body || node.body_note) && (
        <div className="app-graph-text">
          {node.version && (
            <div className="app-graph-version">
              <Badge mono variant="outline">
                {'v' + node.version.n}
              </Badge>
              <Text variant="label" tone="muted">
                {shortDate(new Date(node.version.at), new Date(now))}
              </Text>
            </div>
          )}
          {body && <Prose html={body} />}
          {node.body_note && (
            <Text as="p" variant="code" tone="muted">
              {node.body_note}
            </Text>
          )}
        </div>
      )}
    </div>
  );
}
