import { useCallback, useEffect, useState } from 'react';
import { useNavigate } from 'react-router';
import { ApiError, apiGet, apiPost } from '../../api/client';
import type { SeedCoSRequest, Setup as SetupData, SetupProgress, SetupStep } from '../../api/types.gen';
import { KivaliLockup } from '../../app/Bare';
import { Progress, Text } from '../../ds';
import { cx } from '../../lib/cx';
import { useDocumentTitle } from '../../lib/documentTitle';
import { CosStep, NO_DRAFTS } from './CosStep';
import type { Drafts } from './CosStep';
import { DoneStep } from './DoneStep';
import { FilesStep } from './FilesStep';
import { HiringPanel } from './HiringPanel';
import { OrgStep } from './OrgStep';
import { ErrorBanner } from './parts';
import { WelcomeStep } from './WelcomeStep';
import '../../styles/setup.css';

const STEP_LABELS = ['Welcome', 'Your org', 'Project files', 'Chief of Staff'];
const STEP_NUMBER: Record<SetupStep, number> = { welcome: 1, org: 2, files: 3, cos: 4, done: 4 };

export interface SetupProps {
  /** Milliseconds between hire-progress checks. */
  pollMs?: number;
}

/** True while the hire is still going and the panel should keep asking. */
function isPolling(hire: { progress: SetupProgress | null } | null): boolean {
  if (!hire) return false;
  return hire.progress === null || hire.progress.state === 'idle' || hire.progress.state === 'running';
}

/**
 * Setup, outside the frame (canvases 6f to 6k): four steps under a bar. The server says where to resume; every
 * write answers with the whole Setup, which replaces what is on screen. Back and Continue move between steps
 * without the server; the hire is the one long call, followed by polling /progress.
 */
export function Setup({ pollMs = 1000 }: SetupProps) {
  useDocumentTitle('Set up');
  const navigate = useNavigate();
  const [setup, setSetup] = useState<SetupData | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [view, setView] = useState<SetupStep>('welcome');
  const [fromFiles, setFromFiles] = useState(true);
  const [drafts, setDrafts] = useState<Drafts>(NO_DRAFTS);
  const [hire, setHire] = useState<{ progress: SetupProgress | null } | null>(null);
  const [starting, setStarting] = useState(false);
  const [cosError, setCosError] = useState<unknown>(null);
  const [pollTick, setPollTick] = useState(0);

  // Resume where the server says, and pick up a hire already in flight.
  useEffect(() => {
    const abort = new AbortController();
    apiGet<SetupData>('/api/v1/setup', { signal: abort.signal })
      .then(async (s) => {
        setSetup(s);
        setView(s.step);
        if (s.step !== 'cos' && s.step !== 'done') return;
        try {
          const p = await apiGet<SetupProgress>('/api/v1/setup/progress', { signal: abort.signal });
          if (p.state === 'done') setView('done');
          else if (p.state === 'running' || p.state === 'failed') {
            setView('cos');
            setHire({ progress: p });
          }
        } catch {
          // No progress to show; the step itself is enough.
        }
      })
      .catch((err: unknown) => {
        if (!(err instanceof DOMException && err.name === 'AbortError')) setLoadError(err);
      });
    return () => abort.abort();
  }, []);

  // While the hire runs: ask right away, then every pollMs, until it is done or failed.
  const polling = isPolling(hire);
  const lastProgress = hire?.progress ?? null;
  useEffect(() => {
    if (!polling) return;
    let cancelled = false;
    const timer = setTimeout(
      () => {
        apiGet<SetupProgress>('/api/v1/setup/progress')
          .then((p) => {
            if (cancelled) return;
            setHire({ progress: p });
            if (p.state === 'done') setView('done');
          })
          .catch(() => {
            // A missed check is not a failure; ask again.
            if (!cancelled) setPollTick((n) => n + 1);
          });
      },
      lastProgress === null ? 0 : pollMs,
    );
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [polling, lastProgress, pollTick, pollMs]);

  const [checking, setChecking] = useState(false);
  const recheck = useCallback(() => {
    setChecking(true);
    setCosError(null);
    apiGet<SetupData>('/api/v1/setup')
      .then(setSetup)
      .catch((err: unknown) => setCosError(err))
      .finally(() => setChecking(false));
  }, []);

  if (!setup) {
    return (
      <div className="app-setup">
        <SetupBar step={null} />
        <main className="app-setup-main">
          <div className="app-setup-column">{loadError != null && <ErrorBanner error={loadError} />}</div>
        </main>
      </div>
    );
  }

  const files = setup.files.length;
  const personal = setup.kind === 'personal';
  // A personal team's Chief of Staff always drafts About you; the server ignores the flag for it.
  const effectiveFromFiles = fromFiles && files > 0 && !personal;

  async function startHire() {
    if (!setup) return;
    const body: SeedCoSRequest = { handbook_from_files: effectiveFromFiles };
    if (drafts.role !== null && drafts.role !== setup.cos.default_role_md) body.role_md = drafts.role;
    if (drafts.handbook !== null && drafts.handbook !== setup.cos.default_handbook_md) body.handbook_md = drafts.handbook;
    setStarting(true);
    setCosError(null);
    try {
      await apiPost('/api/v1/setup/seed-cos', body);
      setHire({ progress: null });
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        // Already being hired (another tab, or a double click) or already hired: follow it; /progress says which.
        setHire({ progress: null });
      } else {
        setHire(null);
        setCosError(err);
      }
    } finally {
      setStarting(false);
    }
  }

  const go = (to: SetupStep) => () => setView(to);
  const stepNumber = STEP_NUMBER[view];

  let body;
  switch (view) {
    case 'welcome':
      body = <WelcomeStep restoreAvailable={setup.restore_available} onStart={go('org')} />;
      break;
    case 'org':
      body = <OrgStep setup={setup} onSetup={setSetup} onBack={go('welcome')} onNext={go('files')} />;
      break;
    case 'files':
      body = <FilesStep setup={setup} fromFiles={fromFiles} onFromFiles={setFromFiles} onSetup={setSetup} onBack={go('org')} onNext={go('cos')} />;
      break;
    case 'cos':
      body = hire ? (
        <HiringPanel
          progress={hire.progress}
          onBack={() => {
            setHire(null);
            setCosError(null);
          }}
          retrying={starting}
          onRetry={() => void startHire()}
        />
      ) : (
        <CosStep
          setup={setup}
          fromFiles={effectiveFromFiles}
          drafts={drafts}
          onDrafts={setDrafts}
          error={cosError}
          hiring={starting}
          checking={checking}
          onBack={go('files')}
          onHire={() => void startHire()}
          onRecheck={recheck}
        />
      );
      break;
    default:
      body = <DoneStep fileCount={files} fromFiles={effectiveFromFiles} personal={personal} onHome={() => void navigate('/')} />;
  }

  return (
    <div className="app-setup">
      <SetupBar step={stepNumber} />
      <main className="app-setup-main">
        <div className="app-setup-column">
          <div className="app-setup-steps">
            <Progress aria-label="Setup" aria-valuetext={'Step ' + stepNumber + ' of 4'} value={stepNumber} max={4} tone="ink" />
            <ol className="app-setup-labels">
              {STEP_LABELS.map((label, i) => (
                <li key={label} className={cx('app-setup-label', i < stepNumber && 'is-reached', i === stepNumber - 1 && 'is-current')} aria-current={i === stepNumber - 1 ? 'step' : undefined}>
                  {label}
                </li>
              ))}
            </ol>
          </div>
          {body}
        </div>
      </main>
    </div>
  );
}

function SetupBar({ step }: { step: number | null }) {
  return (
    <header className="app-setup-bar">
      <KivaliLockup />
      {step !== null && (
        <Text variant="label" tone="muted">
          Step {step} of 4
        </Text>
      )}
    </header>
  );
}
