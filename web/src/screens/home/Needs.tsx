import { useState } from 'react';
import { Link, useNavigate } from 'react-router';
import type { AssignmentResolution, NeedItem } from '../../api/types.gen';
import { Badge, Button, EmptyState, Icon, ListRow, Select, Text, TextField } from '../../ds';
import { pickFiles } from '../../lib/pickFiles';
import { relativeTime } from '../../lib/time';
import { needKindLabel } from '../../state/home';
import { Attachments, Body, Face } from './parts';

/** What an expanded Needs-you row sends. */
export type NeedAction =
  | { kind: 'answer'; verb: 'approve' | 'deny' | 'ack'; message: string; files: File[] }
  | { kind: 'close'; resolution: AssignmentResolution; outcome: string };

const RESOLUTIONS = [
  { value: 'done', label: 'Done' },
  { value: 'dropped', label: 'Dropped' },
];

function AttachedFiles({ files }: { files: File[] }) {
  return (
    <>
      {files.map((f, i) => (
        <Badge key={f.name + i} variant="outline" mono>
          {f.name}
        </Badge>
      ))}
    </>
  );
}

function ApprovalForm({ onAct }: { onAct(a: NeedAction): void }) {
  const [message, setMessage] = useState('');
  const [files, setFiles] = useState<File[]>([]);
  const attach = async () => {
    const picked = await pickFiles();
    if (picked.length > 0) setFiles((prev) => [...prev, ...picked]);
  };
  return (
    <>
      <TextField label="Note (optional)" multiline rows={2} placeholder="Go ahead, and keep me posted." value={message} onChange={(e) => setMessage(e.currentTarget.value)} />
      <div className="app-home-actions">
        <Button variant="ghost" size="sm" icon={<Icon name="paperclip" />} onClick={() => void attach()}>
          Attach
        </Button>
        <AttachedFiles files={files} />
        <span className="app-home-spacer" />
        <Button variant="secondary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'deny', message, files })}>
          Deny
        </Button>
        <Button variant="primary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'approve', message, files })}>
          Approve
        </Button>
      </div>
    </>
  );
}

function AckForm({ onAct }: { onAct(a: NeedAction): void }) {
  const [message, setMessage] = useState('');
  return (
    <>
      <TextField label="Reply (optional)" multiline rows={2} placeholder="Thanks. Keep me posted." value={message} onChange={(e) => setMessage(e.currentTarget.value)} />
      <div className="app-home-actions">
        <span className="app-home-spacer" />
        <Button variant="primary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'ack', message, files: [] })}>
          Acknowledge
        </Button>
      </div>
    </>
  );
}

function HandBackForm({ assignmentId, onAct }: { assignmentId: number | undefined; onAct(a: NeedAction): void }) {
  const [resolution, setResolution] = useState<AssignmentResolution>('done');
  const [outcome, setOutcome] = useState('');
  const [error, setError] = useState<string | undefined>(undefined);
  const submit = () => {
    if (!outcome.trim()) {
      setError('Say what was done, or why it is dropped.');
      return;
    }
    onAct({ kind: 'close', resolution, outcome: outcome.trim() });
  };
  return (
    <>
      <div className="app-home-handback">
        <Select label="Resolution" options={RESOLUTIONS} value={resolution} onChange={(e) => setResolution(e.currentTarget.value === 'dropped' ? 'dropped' : 'done')} />
        <TextField
          label="Outcome"
          placeholder="Confirmed, and the ledger is updated."
          value={outcome}
          error={error}
          onChange={(e) => {
            setOutcome(e.currentTarget.value);
            if (error) setError(undefined);
          }}
        />
      </div>
      <div className="app-home-actions">
        {assignmentId !== undefined && (
          <Link className="app-home-link" to={'/assignments/' + assignmentId}>
            Open #{assignmentId}
          </Link>
        )}
        <span className="app-home-spacer" />
        <Button variant="primary" size="sm" onClick={submit}>
          Hand back
        </Button>
      </div>
    </>
  );
}

interface NeedRowProps {
  item: NeedItem;
  now: number;
  base: string;
  expanded: boolean;
  onToggle(): void;
  onAct(a: NeedAction): void;
}

function NeedRow({ item, now, base, expanded, onToggle, onAct }: NeedRowProps) {
  const navigate = useNavigate();
  const review = item.kind === 'proposal';
  const who = item.kind === 'needs_help' && item.agent ? item.agent : item.from;
  const meta = (
    <>
      <span>{item.from.name}</span>
      <Badge>{needKindLabel(item)}</Badge>
      <span>{relativeTime(item.at, now)}</span>
    </>
  );
  const trail = review ? (
    <span className="app-home-review">
      Review
      <Icon name="arrow-right" />
    </span>
  ) : (
    <Icon name={expanded ? 'chevron-up' : 'chevron-down'} />
  );
  const lead = <Face who={who} size={32} />;
  return (
    <div className="app-home-item" data-id={item.id}>
      {review ? (
        <ListRow lead={lead} title={item.title} meta={meta} trail={trail} href={item.review_path ?? base + '/'} />
      ) : (
        <ListRow lead={lead} title={item.title} meta={meta} trail={trail} onClick={onToggle} expanded={expanded} />
      )}
      {expanded && !review && (
        <div className="app-home-expand">
          <Body markdown={item.body_md} />
          <Attachments items={item.attachments} />
          {item.kind === 'approval' && <ApprovalForm onAct={onAct} />}
          {item.kind === 'notification' && <AckForm onAct={onAct} />}
          {item.kind === 'assignment' && <HandBackForm assignmentId={item.assignment?.id} onAct={onAct} />}
          {item.kind === 'needs_help' && item.agent && (
            <div className="app-home-actions">
              <span className="app-home-spacer" />
              {/* Secondary, as canvas 1c draws it: Open chat goes somewhere, it does not decide anything. */}
              <Button variant="secondary" size="sm" onClick={() => void navigate('/agents/' + encodeURIComponent(item.agent?.slug ?? ''))}>
                Open chat
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export interface NeedsProps {
  items: readonly NeedItem[];
  now: number;
  base: string;
  expanded: ReadonlySet<string>;
  onToggle(id: string): void;
  onAct(item: NeedItem, action: NeedAction): void;
}

/** Needs you: the view's one signal highlight is the count beside the heading. */
export function Needs({ items, now, base, expanded, onToggle, onAct }: NeedsProps) {
  return (
    <section className="app-home-section" aria-labelledby="home-needs">
      <div className="app-home-head">
        <Text as="h2" variant="heading" id="home-needs">
          Needs you
        </Text>
        {items.length > 0 ? (
          <Badge tone="signal" variant="solid" mono aria-label={items.length + ' waiting on you'}>
            {items.length}
          </Badge>
        ) : (
          <Text variant="label" tone="muted">
            nothing waiting
          </Text>
        )}
      </div>
      <div className="app-home-list">
        <div className="app-home-rows">
          {items.length === 0 ? (
            <EmptyState title="Nothing waiting on you">Approvals, questions and agents that need help land here.</EmptyState>
          ) : (
            items.map((n) => (
              <NeedRow key={n.id} item={n} now={now} base={base} expanded={expanded.has(n.id)} onToggle={() => onToggle(n.id)} onAct={(a) => onAct(n, a)} />
            ))
          )}
        </div>
      </div>
    </section>
  );
}
