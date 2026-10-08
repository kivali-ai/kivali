import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { apiPost } from '../../api/client';
import type { ApiError } from '../../api/client';
import type { Assignment, AssignmentCloseRequest, AssignmentHoldRequest, AssignmentReopenRequest, AssignmentResolution, AssignmentUpdateRequest } from '../../api/types.gen';
import { Banner, Button, Dialog, DialogClose, Select, TextField } from '../../ds';
import { asSentence } from '../../state/home';
import { flattenTree } from '../../state/org';
import { useOrg } from '../../state/OrgProvider';
import { YOU, asApiError } from './parts';

export type ActKind = 'close' | 'hold' | 'release' | 'reopen' | 'edit';

/** What each action says in a toast once the server has taken it. */
const DONE: Record<ActKind, string> = {
  close: 'Assignment closed',
  hold: 'Put on hold',
  release: 'Hold released',
  reopen: 'Assignment reopened',
  edit: 'Changes saved',
};

export const STALE_TITLE = 'This assignment changed while you were editing.';
export const STALE_BODY = 'Reload to see the latest.';

const RESOLUTIONS: { value: AssignmentResolution; label: string }[] = [
  { value: 'done', label: 'Done' },
  { value: 'dropped', label: 'Dropped' },
];

/** "Test runner and Engineering lead are": the people an action tells, without you, with the verb that agrees. */
function told(a: Assignment): string {
  const names: string[] = [];
  for (const who of [a.facts.assignee, a.facts.opened_by]) {
    if (who.slug !== YOU && who.name && !names.includes(who.name)) names.push(who.name);
  }
  if (names.length === 0) return 'Everyone involved is';
  if (names.length === 1) return names[0] + ' is';
  return names.join(' and ') + ' are';
}

function assigneeName(a: Assignment): string {
  const who = a.facts.assignee;
  return who.slug === YOU || !who.name ? 'The assignee' : who.name;
}

export interface ActDialogProps {
  kind: ActKind;
  assignment: Assignment;
  /** The seq the edit was opened against; it goes back with an edit so a changed record is refused. */
  seq: number;
  onClose(): void;
  /** The server took the action: the screen refetches and toasts. */
  onDone(message: string): void;
  /** The person chose Reload after a stale edit. */
  onReload(): void;
}

interface Shell {
  title: string;
  description?: ReactNode;
  primary: string;
  busy: boolean;
  disabled: boolean;
  error: ApiError | null;
  stale: boolean;
  onSubmit(): void;
  onClose(): void;
  onReload(): void;
  children: ReactNode;
}

function DialogShell({ title, description, primary, busy, disabled, error, stale, onSubmit, onClose, onReload, children }: Shell) {
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      title={title}
      description={description}
      footer={
        <>
          <DialogClose asChild>
            <Button variant="secondary">Cancel</Button>
          </DialogClose>
          <Button variant="primary" loading={busy} disabled={busy || disabled} onClick={onSubmit}>
            {primary}
          </Button>
        </>
      }
    >
      <div className="app-work-form">
        {stale ? (
          <Banner
            tone="warning"
            title={STALE_TITLE}
            action={
              <Button size="sm" variant="secondary" onClick={onReload}>
                Reload
              </Button>
            }
          >
            {STALE_BODY}
          </Banner>
        ) : (
          error && (
            <Banner tone="danger" title={asSentence(error.message)}>
              {error.who}
            </Banner>
          )
        )}
        {children}
      </div>
    </Dialog>
  );
}

/** One Act dialog. Cancel is secondary; the primary is named for the action. */
export function ActDialog({ kind, assignment, seq, onClose, onDone, onReload }: ActDialogProps) {
  const { org } = useOrg();
  const base = '/api/v1/assignments/' + assignment.id;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [stale, setStale] = useState(false);

  const [resolution, setResolution] = useState<AssignmentResolution>('done');
  const [outcome, setOutcome] = useState('');
  const [note, setNote] = useState('');
  const [title, setTitle] = useState(assignment.title);
  const [description, setDescription] = useState(assignment.description_md);
  const [assignee, setAssignee] = useState(assignment.facts.assignee.slug);

  const assignees = useMemo(() => {
    const options = flattenTree(org.tree).map((n) => ({ value: n.slug, label: n.name }));
    const current = assignment.facts.assignee;
    if (current.slug && !options.some((o) => o.value === current.slug)) options.unshift({ value: current.slug, label: current.slug === YOU ? 'You' : current.name });
    return options;
  }, [org.tree, assignment.facts.assignee]);

  const submit = async (path: string, body: unknown) => {
    setBusy(true);
    setError(null);
    try {
      await apiPost(base + path, body);
      onDone(DONE[kind]);
    } catch (err) {
      const e = asApiError(err);
      // Only an edit carries a seq, so a 409 there means the record moved on.
      if (kind === 'edit' && e.status === 409) setStale(true);
      else setError(e);
      setBusy(false);
    }
  };

  const shell = { busy, error, stale, onClose, onReload };
  const id = '#' + assignment.id;
  const who = assigneeName(assignment);

  if (kind === 'close') {
    const body: AssignmentCloseRequest = { resolution, outcome: outcome.trim() };
    return (
      <DialogShell
        {...shell}
        title={'Close ' + id + '?'}
        description={told(assignment) + ' told at once. The outcome is what everyone reads later, so say what was decided.'}
        // Not the canvas's bare "Close": the dialog's own dismiss button is already named Close.
        primary="Close assignment"
        disabled={!outcome.trim()}
        onSubmit={() => void submit('/close', body)}
      >
        <Select label="Resolution" options={RESOLUTIONS} value={resolution} onChange={(e) => setResolution(e.target.value as AssignmentResolution)} />
        <TextField label="Outcome" multiline rows={3} value={outcome} onChange={(e) => setOutcome(e.target.value)} placeholder="Confirmed, account ending 4471." />
      </DialogShell>
    );
  }

  if (kind === 'hold' || kind === 'release') {
    const held = kind === 'hold';
    const body: AssignmentHoldRequest = { held, note: note.trim() };
    return (
      <DialogShell
        {...shell}
        title={held ? 'Put ' + id + ' on hold?' : 'Release the hold on ' + id + '?'}
        description={
          held
            ? who + ' stops working on it until you resume it. Anything waiting on ' + id + ' stays blocked, and the row will say "On hold by you".'
            : who + ' picks it up again. Anything that was waiting only on the hold can move.'
        }
        primary={held ? 'Put on hold' : 'Release hold'}
        disabled={!note.trim()}
        onSubmit={() => void submit('/hold', body)}
      >
        <TextField
          label="Note"
          hint={'Required. ' + who + ' reads it first.'}
          multiline
          rows={3}
          value={note}
          onChange={(e) => setNote(e.target.value)}
          placeholder={held ? 'Hold until the review is done on Thursday.' : 'The review is done. Carry on.'}
        />
      </DialogShell>
    );
  }

  if (kind === 'reopen') {
    const body: AssignmentReopenRequest = { note: note.trim() };
    return (
      <DialogShell
        {...shell}
        title={'Reopen ' + id + '?'}
        description={who + ' reads your note first, then picks it up again.'}
        primary="Reopen"
        disabled={!note.trim()}
        onSubmit={() => void submit('/reopen', body)}
      >
        <TextField label="Note" hint="Required. Say what is wrong with the outcome." multiline rows={3} value={note} onChange={(e) => setNote(e.target.value)} placeholder="The tests ran on staging, not production." />
      </DialogShell>
    );
  }

  // edit: only what changed goes back, with the seq the edit was opened against.
  const body: AssignmentUpdateRequest = { seq };
  if (title.trim() !== assignment.title) body.title = title.trim();
  if (description !== assignment.description_md) body.description_md = description;
  if (assignee !== assignment.facts.assignee.slug) body.assignee = assignee;
  if (note.trim()) body.note = note.trim();
  const changed = body.title !== undefined || body.description_md !== undefined || body.assignee !== undefined;
  return (
    <DialogShell {...shell} title={'Edit ' + id} primary="Save changes" disabled={!changed || !title.trim()} onSubmit={() => void submit('/update', body)}>
      <TextField label="Title" value={title} onChange={(e) => setTitle(e.target.value)} />
      <TextField label="Description" multiline rows={6} value={description} onChange={(e) => setDescription(e.target.value)} />
      <Select label="Assignee" options={assignees} value={assignee} onChange={(e) => setAssignee(e.target.value)} />
      <TextField label="Note" hint="Required when the change wakes someone else, such as a new assignee." multiline rows={2} value={note} onChange={(e) => setNote(e.target.value)} />
    </DialogShell>
  );
}
