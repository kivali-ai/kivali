import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { apiGet, apiPost } from '../../api/client';
import type { ApiError } from '../../api/client';
import { createEventStream } from '../../api/sse';
import type { EventSourceLike } from '../../api/sse';
import type { SetupFlow, SetupFlowAnswer, SetupFlowStart } from '../../api/types.gen';
import { Banner, Button, Icon, Progress, Text, TextField } from '../../ds';
import { ErrorBanner } from './parts';
import { asApiError } from './useResource';

/** How often the panel asks how a flow stands, where it cannot follow the flow's stream. */
export const SETUP_POLL_MS = 3000;

/** The states of a flow that has not ended: the panel follows it. */
const UNDER_WAY: ReadonlySet<SetupFlow['state']> = new Set(['running', 'form', 'go', 'wait']);

/** How long a code works, from its expiry: "The code works for 10 minutes." */
export function codeLifeWords(expiresAt: string, now: number): string {
  const minutes = Math.max(1, Math.round((Date.parse(expiresAt) - now) / 60_000));
  return 'The code works for ' + minutes + (minutes === 1 ? ' minute.' : ' minutes.');
}

/** "Building the app, 3 of 5": a running step's progress in words. */
export function progressWords(p: NonNullable<SetupFlow['progress']>): string {
  return p.total > 0 ? p.words + ', ' + p.done + ' of ' + p.total : p.words;
}

/** Whether the tab is showing; a hidden one asks the server nothing. */
export function useVisible(): boolean {
  const [visible, setVisible] = useState(() => document.visibilityState !== 'hidden');
  useEffect(() => {
    const onVisibility = () => setVisible(document.visibilityState !== 'hidden');
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  }, []);
  return visible;
}

/**
 * Sends the browser on a flow's Go step. A step the service sends the browser back from leaves this page (Kivali
 * Desktop opens the system browser instead and the page stays); one it sends nowhere opens in a new tab, so this page
 * stays to follow the flow.
 */
export function goTo(startURL: string, noReturn: boolean): void {
  if (noReturn) window.open(startURL, '_blank', 'noopener');
  else window.location.assign(startURL);
}

export interface SetupFlowPanelProps {
  /** The integration's routes: /api/v1/org/integrations/{name}. */
  api: string;
  /** The integration's name in words, which a form's fields are asked under. */
  integration: string;
  /** The panel's heading and its button that starts the flow: Connect, Sign in, Set up. */
  label: string;
  /** What starting it is called where it differs from the heading (Sign in with a code). */
  startLabel?: string;
  /** A line under the heading, such as where a sign-in goes ("Signs in at github.com"). */
  note?: string;
  busy?: boolean;
  /** The flow a service's callback sent the browser back with (?setup=<id>), read once. */
  returned?: string | null;
  /** Reads the page again once the flow is done; settles once it is in. */
  onDone(): Promise<unknown>;
  /** Opens the flow's stream; tests pass a fake. Without one, and without EventSource, the panel polls. */
  openStream?: (url: string) => EventSourceLike;
}

/**
 * A setup flow, drawn from its state as the server streams it: the button that starts it; while it runs, its progress
 * (a real progress bar where there is a count); a Form of the fields the integration declared, secrets as password
 * fields; a Go that sends the browser to the service, by /connect/start, which Kivali Desktop opens in the system
 * browser; a Wait with its words, a code to copy and the service's page; done; or failed with why and Try again.
 * Cancel ends a flow under way. The panel follows the flow's stream, or asks every few seconds while the tab is
 * visible where there is no stream.
 */
export function SetupFlowPanel({ api, integration, label, startLabel, note, busy = false, returned, onDone, openStream }: SetupFlowPanelProps) {
  const [flow, setFlow] = useState<SetupFlow | null>(null);
  // A flow the callback named that the server no longer has: it took too long, or Kivali restarted.
  const [gone, setGone] = useState(false);
  const [values, setValues] = useState<Record<string, string>>({});
  const [error, setError] = useState<ApiError | null>(null);
  const [working, setWorking] = useState(false);
  const [went, setWent] = useState(false);
  const [copied, setCopied] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const visible = useVisible();
  const done = useRef(onDone);
  useEffect(() => {
    done.current = onDone;
  });
  const finished = useRef<string | null>(null);
  const shown = useRef('');

  // Applies a state from the server: a new step starts with a blank form, and a flow done reads the page again, once.
  const take = (next: SetupFlow | null) => {
    const key = next ? next.id + ':' + next.state : '';
    if (key !== shown.current) {
      shown.current = key;
      setValues({});
      setCopied(false);
      setNow(Date.now());
    }
    setFlow(next);
    if (next?.state === 'done' && finished.current !== next.id) {
      finished.current = next.id;
      void done.current();
    }
  };

  // The flow under way, or the one that last ended, as the page opens.
  useEffect(() => {
    let stopped = false;
    apiGet<SetupFlow>(api + '/setup').then(
      (f) => {
        if (!stopped) take(f);
      },
      (err: unknown) => {
        if (stopped) return;
        const e = asApiError(err);
        if (e.status === 404) {
          if (returned) setGone(true);
        } else setError(e);
      },
    );
    return () => {
      stopped = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- read once per integration
  }, [api]);

  const id = flow && UNDER_WAY.has(flow.state) ? flow.id : null;
  useEffect(() => {
    if (!id) return;
    const reread = () =>
      apiGet<SetupFlow>(api + '/setup').then(take, (err: unknown) => {
        const e = asApiError(err);
        if (e.status === 404) {
          take(null);
          setGone(true);
        } else setError(e);
      });
    if (openStream || typeof EventSource !== 'undefined') {
      const stream = createEventStream(api + '/setup/stream', openStream ? { open: openStream } : {});
      const off = stream.on('flow', (data) => take(JSON.parse(data) as SetupFlow));
      // A stream that closed for good (the flow forgotten): what is there now.
      const offState = stream.onState((s) => {
        if (s === 'closed') void reread();
      });
      stream.start();
      return () => {
        off();
        offState();
        stream.stop();
      };
    }
    if (!visible) return;
    const timer = setInterval(() => void reread(), SETUP_POLL_MS);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- take is stable in effect
  }, [id, visible, api, openStream]);

  const run = async (fn: () => Promise<void>) => {
    setWorking(true);
    setError(null);
    try {
      await fn();
    } catch (err) {
      setError(asApiError(err));
    } finally {
      setWorking(false);
    }
  };
  const start = () =>
    run(async () => {
      setGone(false);
      setWent(false);
      const body: SetupFlowStart = { flow: 'setup' };
      take(await apiPost<SetupFlow>(api + '/setup', body));
    });
  const answer = () =>
    run(async () => {
      const body: SetupFlowAnswer = { fields: values };
      take(await apiPost<SetupFlow>(api + '/setup/answer', body));
    });
  const cancel = () =>
    run(async () => {
      await apiPost(api + '/setup/cancel');
      take(null);
    });
  const copy = (code: string) => {
    void navigator.clipboard?.writeText(code).then(
      () => setCopied(true),
      () => setCopied(false),
    );
  };

  const disabled = busy || working;
  const cancelButton = (
    <Button type="button" variant="ghost" disabled={disabled} onClick={() => void cancel()}>
      Cancel
    </Button>
  );
  let body: ReactNode;
  const step = flow?.step;
  if (!flow || flow.state === 'done') {
    body = (
      <>
        {gone && (
          <Banner tone="danger" title="This setup is no longer under way">
            It took too long, or Kivali restarted. Start it again.
          </Banner>
        )}
        {flow?.state === 'done' ? (
          <Text as="p" variant="body">
            Done.
          </Text>
        ) : (
          <div className="app-org-actions">
            <Button type="button" variant="primary" disabled={disabled} onClick={() => void start()}>
              {startLabel ?? label}
            </Button>
          </div>
        )}
      </>
    );
  } else if (flow.state === 'failed') {
    body = (
      <>
        <Banner tone="danger" title="Setup did not finish">
          {flow.error ?? 'Kivali was not told why.'}
        </Banner>
        <div className="app-org-actions">
          <Button type="button" variant="primary" disabled={disabled} onClick={() => void start()}>
            Try again
          </Button>
        </div>
      </>
    );
  } else if (flow.state === 'form' && step) {
    body = (
      <form
        className="app-int-form"
        onSubmit={(e) => {
          e.preventDefault();
          void answer();
        }}
      >
        {step.words && (
          <Text as="p" variant="body">
            {step.words}
          </Text>
        )}
        <Text as="p" variant="caption" tone="muted">
          {integration + ' asks for'}
        </Text>
        {step.fields.map((f) => (
          <TextField
            key={f.key}
            label={f.title}
            hint={f.setup_only ? f.words + ' Used during setup and never kept.' : f.words}
            type={f.secret ? 'password' : 'text'}
            autoComplete="off"
            value={values[f.key] ?? ''}
            onChange={(e) => {
              const v = e.currentTarget.value;
              setValues((m) => ({ ...m, [f.key]: v }));
            }}
          />
        ))}
        <div className="app-org-actions">
          <Button type="submit" variant="primary" disabled={disabled}>
            Continue
          </Button>
          {cancelButton}
        </div>
      </form>
    );
  } else if (flow.state === 'go' && step) {
    body = (
      <>
        <Text as="p" variant="body">
          {step.words}
        </Text>
        {went && (
          // Kivali Desktop opens no browser for a team elsewhere (it could not come back there): this says what to do.
          <Text as="p" variant="caption" tone="muted">
            If no browser opened, open the team in your browser and continue from there.
          </Text>
        )}
        <div className="app-org-actions">
          <Button
            type="button"
            variant="primary"
            disabled={disabled}
            onClick={() => {
              setWent(true);
              goTo(step.start_url ?? '', step.no_return);
            }}
          >
            {step.host ? 'Continue to ' + step.host : 'Continue'}
          </Button>
          {cancelButton}
        </div>
      </>
    );
  } else if (flow.state === 'wait' && step) {
    body = (
      <div className="app-int-device">
        <Text as="p" variant="body">
          {step.words}
        </Text>
        {step.code && (
          <div className="app-int-device-code">
            <Text variant="display" className="app-int-code" aria-label="Code">
              {step.code}
            </Text>
            <Button type="button" size="sm" variant="ghost" icon={<Icon name={copied ? 'check' : 'copy'} />} onClick={() => copy(step.code ?? '')}>
              {copied ? 'Copied' : 'Copy code'}
            </Button>
          </div>
        )}
        {step.url && (
          <Text as="p" variant="body">
            {/* eslint-disable-next-line react/forbid-elements -- external */}
            <a href={step.url} target="_blank" rel="noopener">
              {step.host ?? step.url}
            </a>
          </Text>
        )}
        <Text as="p" variant="caption" tone="muted">
          {(step.code ? codeLifeWords(flow.expires_at, now) + ' ' : '') + 'Waiting.'}
        </Text>
        <div className="app-org-actions">{cancelButton}</div>
      </div>
    );
  } else {
    const p = flow.progress;
    body = (
      <>
        {p && p.total > 0 ? (
          <Progress value={p.done} max={p.total} label={p.words} aria-valuetext={progressWords(p)} />
        ) : (
          <Text as="p" variant="body" tone="muted">
            {p?.words ?? 'Working.'}
          </Text>
        )}
        <div className="app-org-actions">{cancelButton}</div>
      </>
    );
  }
  return (
    <section className="app-int-sign-in" aria-label={label}>
      <Text as="h3" variant="label" tone="muted">
        {label}
      </Text>
      {note && (
        <Text as="p" variant="caption" tone="muted">
          {note}
        </Text>
      )}
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {body}
    </section>
  );
}
