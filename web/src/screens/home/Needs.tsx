import { Fragment, useState } from 'react';
import { Link, useHref, useNavigate } from 'react-router';
import type { AssignmentResolution, AttachmentRef, IntegrationHoldRef, NeedItem, SubagentRef, WebBudgetRef, WebUploadRef } from '../../api/types.gen';
import { browserWindowName, browserWindowPath, subagentPath } from '../../app/links';
import { Badge, Button, EmptyState, Icon, ListRow, Select, Text, TextField } from '../../ds';
import { pickFiles } from '../../lib/pickFiles';
import { relativeTime } from '../../lib/time';
import { needKindLabel, signInPageLine } from '../../state/home';
import { SoftwarePackages } from '../proposals/SoftwarePackages';
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

/** The approve button's words: Install for an Install software request, as on the proposal page. */
function ApprovalForm({ approve = 'Approve', onAct }: { approve?: string; onAct(a: NeedAction): void }) {
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
          {approve}
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

/**
 * A web task waiting for a sign-in: the task it is, the way to its browser window and the way to its page. No
 * answer goes back; the row leaves once the task resumes or ends.
 */
function SignInActions({ task }: { task: SubagentRef }) {
  const browserHref = useHref(browserWindowPath(task.parent, task.id));
  const page = signInPageLine(task);
  return (
    <>
      <Text as="p" variant="caption" tone="muted">
        {'Task: ' + task.description}
      </Text>
      {page !== '' && (
        // The page's own title and address, as the browser reported them: text to read, never a link to follow.
        <Text as="p" variant="caption" tone="muted">
          {'Page: ' + page}
        </Text>
      )}
      <div className="app-home-actions">
        <Link className="app-home-link" to={subagentPath(task.parent, task.id)}>
          Open task
        </Link>
        <span className="app-home-spacer" />
        {/* The task view's window: one per task under a fixed name, opened without noopener so the name finds it. */}
        <Button variant="primary" size="sm" onClick={() => void window.open(browserHref, browserWindowName(task.id), 'width=1320,height=900')}>
          Open browser
        </Button>
      </div>
    </>
  );
}

/**
 * A web task that wants to upload files: what it uploads, where it starts and the prompt its agent gave it, a
 * download of each file just above the note, then deny or approve. The prompt is the agent's words, shown as plain text.
 */
function UploadApproval({ upload, files, onAct }: { upload: WebUploadRef; files: readonly AttachmentRef[]; onAct(a: NeedAction): void }) {
  const [message, setMessage] = useState('');
  return (
    <>
      <dl className="app-home-upload">
        <dt>Task</dt>
        <dd>
          <Link className="app-home-link" to={subagentPath(upload.parent, upload.id)}>
            {upload.description}
          </Link>
        </dd>
        <dt>Files</dt>
        <dd>
          {upload.files.map((f) => (
            <Badge key={f} variant="outline" mono>
              {f}
            </Badge>
          ))}
        </dd>
        <dt>Starts at</dt>
        <dd>
          <Text variant="code">{upload.start_url}</Text>
        </dd>
        <dt>Prompt</dt>
        <dd>
          <Text as="p" variant="caption" className="app-home-prompt">
            {upload.prompt}
          </Text>
        </dd>
      </dl>
      <Attachments items={files} />
      <TextField label="Note (optional)" multiline rows={2} placeholder="Say why, if you deny it." value={message} onChange={(e) => setMessage(e.currentTarget.value)} />
      <div className="app-home-actions">
        <span className="app-home-spacer" />
        <Button variant="secondary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'deny', message, files: [] })}>
          Deny
        </Button>
        <Button variant="primary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'approve', message, files: [] })}>
          Approve upload
        </Button>
      </div>
    </>
  );
}

/**
 * An agent's call of an integration that waits for the person before it runs: the taint line first when the agent
 * read outside mail or documents before asking, the integration and action, whom it reaches, each field exactly as it
 * will go, then deny or approve with a note. The values are the agent's words and may quote an outside message, so
 * they are preformatted text, never Markdown.
 */
function HoldApproval({ hold, onAct }: { hold: IntegrationHoldRef; onAct(a: NeedAction): void }) {
  const [message, setMessage] = useState('');
  return (
    <>
      {hold.taint !== '' && (
        <p className="app-home-taint">
          <span className="app-home-taint-mark">
            <Icon name="triangle-alert" />
          </span>
          <Text variant="caption">{hold.taint}</Text>
        </p>
      )}
      <dl className="app-home-upload">
        <dt>Integration</dt>
        <dd>
          <Link className="app-home-link" to={'/org/integrations/' + encodeURIComponent(hold.integration)}>
            {hold.display}
          </Link>
          <Badge variant="outline" mono>
            {hold.action}
          </Badge>
        </dd>
        {hold.recipients.length > 0 && (
          <>
            <dt>Reaches</dt>
            <dd>
              {hold.recipients.map((r, i) => (
                <Badge key={i + ':' + r} variant="outline" mono>
                  {r}
                </Badge>
              ))}
            </dd>
          </>
        )}
        {hold.fields.map((f, i) => (
          <Fragment key={i + ':' + f.label}>
            <dt>{f.label}</dt>
            <dd>
              <pre className="app-home-hold-value">{f.value}</pre>
            </dd>
          </Fragment>
        ))}
      </dl>
      <TextField label="Note (optional)" multiline rows={2} placeholder="Say why, if you deny it." value={message} onChange={(e) => setMessage(e.currentTarget.value)} />
      <div className="app-home-actions">
        <span className="app-home-spacer" />
        <Button variant="secondary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'deny', message, files: [] })}>
          Deny
        </Button>
        <Button variant="primary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'approve', message, files: [] })}>
          Approve
        </Button>
      </div>
    </>
  );
}

/** A web task refused an action over the web budget: the task, then acknowledge. Every word is the server's. */
function BudgetNotice({ notice, onAct }: { notice: WebBudgetRef; onAct(a: NeedAction): void }) {
  return (
    <>
      <Text as="p" variant="caption" tone="muted">
        {'Task: ' + notice.description}
      </Text>
      <div className="app-home-actions">
        <Link className="app-home-link" to={subagentPath(notice.parent, notice.id)}>
          Open task
        </Link>
        <span className="app-home-spacer" />
        <Button variant="primary" size="sm" onClick={() => onAct({ kind: 'answer', verb: 'ack', message: '', files: [] })}>
          Acknowledge
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
          {item.packages.length > 0 && (
            <SoftwarePackages packages={item.packages} />
          )}
          <Body markdown={item.body_md} />
          {item.kind !== 'web_upload' && <Attachments items={item.attachments} />}
          {item.kind === 'approval' && <ApprovalForm approve={item.proposal_kind === 'software_install' ? 'Install' : 'Approve'} onAct={onAct} />}
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
          {item.kind === 'web_sign_in' && item.subagent && <SignInActions task={item.subagent} />}
          {item.kind === 'web_upload' && item.web_upload && <UploadApproval upload={item.web_upload} files={item.attachments} onAct={onAct} />}
          {item.kind === 'web_budget' && item.web_budget && <BudgetNotice notice={item.web_budget} onAct={onAct} />}
          {item.kind === 'integration_hold' && item.integration_hold && <HoldApproval hold={item.integration_hold} onAct={onAct} />}
          {item.kind === 'integration_requirement' && item.review_path && (
            <div className="app-home-actions">
              <span className="app-home-spacer" />
              {/* Nothing to answer here: the row leaves once the requirement is met, from its Fix on the integration's page. */}
              <Button variant="primary" size="sm" onClick={() => void navigate(item.review_path ?? '/')}>
                Open
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
