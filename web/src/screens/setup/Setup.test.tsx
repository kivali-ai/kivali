import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, apiDelete, apiGet, apiPost, apiPostForm } from '../../api/client';
import type { Setup as SetupData, SetupProgress } from '../../api/types.gen';
import { pickFiles } from '../../lib/pickFiles';
import { goProgress, goProgressFailed, goSetup } from './fixtures';
import { Setup } from './Setup';

vi.mock('../../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../api/client')>();
  return { ...real, apiGet: vi.fn(), apiPost: vi.fn(), apiPostForm: vi.fn(), apiDelete: vi.fn() };
});

vi.mock('../../lib/pickFiles', () => ({ pickFiles: vi.fn() }));

const getMock = vi.mocked(apiGet);
const postMock = vi.mocked(apiPost);
const formMock = vi.mocked(apiPostForm);
const deleteMock = vi.mocked(apiDelete);

/** The fixture with the model connected, so the hire button is live. */
const ready: SetupData = { ...goSetup, credential: { ...goSetup.credential, ready: true, present: true, guidance: '' } };

const notFound = () => new ApiError(404, 'not found', 'Nobody');

/** What GET /setup and GET /setup/progress answer; progress is a queue, the last answer repeats. */
function serve(setup: SetupData, progress: SetupProgress[] = []) {
  const queue = [...progress];
  getMock.mockImplementation(((path: string) => {
    if (path === '/api/v1/setup') return Promise.resolve(setup);
    if (path === '/api/v1/setup/progress') {
      const next = queue.length > 1 ? queue.shift() : queue[0];
      return next ? Promise.resolve(next) : Promise.reject(notFound());
    }
    return Promise.reject(notFound());
  }) as typeof apiGet);
}

// Two passes: an answer can schedule the next zero-delay timer (the hire, then its first progress check).
const flush = async () => {
  for (let i = 0; i < 2; i++) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
  }
};

async function renderSetup() {
  const router = createMemoryRouter(
    [
      { path: '/setup/*', element: <Setup pollMs={1000} /> },
      { path: '/', element: <p>Home page</p> },
    ],
    { initialEntries: ['/setup'] },
  );
  render(<RouterProvider router={router} />);
  await flush();
  return router;
}

const click = (name: string | RegExp) => fireEvent.click(screen.getByRole('button', { name }));
const fileInput = () => document.querySelector<HTMLInputElement>('input[type="file"]') as HTMLInputElement;
const upload = (files: File[]) => fireEvent.change(fileInput(), { target: { files } });

beforeEach(() => {
  vi.useFakeTimers();
  getMock.mockReset();
  postMock.mockReset();
  formMock.mockReset();
  deleteMock.mockReset();
});
afterEach(() => vi.useRealTimers());

describe('Setup resume', () => {
  it.each([
    ['welcome', 'Set up your org', 1],
    ['org', 'Your org', 2],
    ['files', 'What your Chief of Staff will read', 3],
    ['cos', 'Hire your Chief of Staff', 4],
  ] as const)('resumes at %s', async (step, heading, n) => {
    serve({ ...ready, step });
    await renderSetup();
    expect(screen.getByRole('heading', { level: 1, name: heading })).toBeInTheDocument();
    expect(screen.getByText('Step ' + n + ' of 4')).toBeInTheDocument();
    const bar = screen.getByRole('progressbar', { name: 'Setup' });
    expect(bar).toHaveAttribute('aria-valuenow', String(n));
    expect(bar).toHaveAttribute('aria-valuetext', 'Step ' + n + ' of 4');
    expect(screen.getAllByRole('heading', { level: 1 })).toHaveLength(1);
    expect(screen.getByRole('img', { name: 'Kivali' })).toBeInTheDocument();
  });

  it('marks the current step label and names all four', async () => {
    serve({ ...goSetup, step: 'files' });
    await renderSetup();
    const labels = screen.getAllByRole('listitem').filter((li) => li.className.includes('app-setup-label'));
    expect(labels.map((l) => l.textContent)).toEqual(['Welcome', 'Your org', 'Project files', 'Chief of Staff']);
    expect(screen.getByText('Project files', { selector: 'li' })).toHaveAttribute('aria-current', 'step');
  });

  it('resumes at done when the hire has finished', async () => {
    serve({ ...goSetup, step: 'done' }, [{ ...goProgress, state: 'done' }]);
    await renderSetup();
    expect(screen.getByRole('heading', { level: 1, name: 'Your Chief of Staff is hired' })).toBeInTheDocument();
  });

  it('resumes into a hire already running', async () => {
    serve({ ...ready, step: 'cos' }, [goProgress]);
    await renderSetup();
    expect(screen.getByText('Hiring your Chief of Staff')).toBeInTheDocument();
    expect(screen.getByText('Writing its first briefing')).toBeInTheDocument();
  });

  it('says what went wrong when the setup cannot be read', async () => {
    getMock.mockRejectedValue(new ApiError(500, 'Setup could not be read.', 'Whoever runs this Kivali server can look into it.'));
    await renderSetup();
    expect(screen.getByText('Setup could not be read.')).toBeInTheDocument();
  });
});

describe('Welcome', () => {
  it('goes to Your org on Start and back again', async () => {
    serve({ ...goSetup, step: 'welcome' });
    await renderSetup();
    click('Start');
    expect(screen.getByRole('heading', { level: 1, name: 'Your org' })).toBeInTheDocument();
    click('Back');
    expect(screen.getByRole('heading', { level: 1, name: 'Set up your org' })).toBeInTheDocument();
  });

  it('restores a backup to the restore route', async () => {
    serve({ ...goSetup, step: 'welcome' });
    formMock.mockResolvedValue({});
    await renderSetup();
    click('Restore from a backup');
    const archive = new File(['zip'], 'kivali-backup.zip');
    upload([archive]);
    await flush();
    expect(formMock).toHaveBeenCalledTimes(1);
    expect(formMock.mock.calls[0]?.[0]).toBe('/api/v1/org/restore');
    expect((formMock.mock.calls[0]?.[1] as FormData).get('archive')).toBe(archive);
    expect(screen.getByText('Backup restored')).toBeInTheDocument();
  });

  it('passes the server refusal through with who can fix it', async () => {
    serve({ ...goSetup, step: 'welcome' });
    formMock.mockRejectedValue(new ApiError(409, 'This org already has data, so a backup cannot be restored over it.', 'Whoever runs this Kivali server can start a fresh one.'));
    await renderSetup();
    click('Restore from a backup');
    upload([new File(['zip'], 'b.zip')]);
    await flush();
    expect(screen.getByText('This org already has data, so a backup cannot be restored over it.')).toBeInTheDocument();
    expect(screen.getByText('Whoever runs this Kivali server can start a fresh one.')).toBeInTheDocument();
  });

  it('refuses a file that is not a backup without sending it', async () => {
    serve({ ...goSetup, step: 'welcome' });
    await renderSetup();
    click('Restore from a backup');
    expect(fileInput()).toHaveAttribute('accept', '.zip,application/zip');
    upload([new File(['x'], 'notes.pdf')]);
    await flush();
    expect(formMock).not.toHaveBeenCalled();
    expect(screen.getByText('That is not a Kivali backup.')).toBeInTheDocument();
  });

  it('hides the restore path when there is nothing to restore into', async () => {
    serve({ ...goSetup, step: 'welcome', restore_available: false });
    await renderSetup();
    expect(screen.queryByRole('button', { name: 'Restore from a backup' })).toBeNull();
  });
});

describe('Your org', () => {
  it('posts the name and moves on, replacing state from the response', async () => {
    serve({ ...goSetup, step: 'org', org: { name: '', has_logo: false, owner_name: '' } });
    postMock.mockResolvedValue({ ...goSetup, org: { name: 'Plainsong Labs', has_logo: false, owner_name: '' } });
    await renderSetup();
    fireEvent.change(screen.getByLabelText('Org name'), { target: { value: '  Plainsong Labs ' } });
    expect(screen.getByRole('img', { name: 'Plainsong Labs' })).toBeInTheDocument();
    click('Continue');
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/setup/org', { name: 'Plainsong Labs' });
    expect(screen.getByRole('heading', { level: 1, name: 'What your Chief of Staff will read' })).toBeInTheDocument();
  });

  it('does not post an unchanged or blank name', async () => {
    serve({ ...goSetup, step: 'org' });
    await renderSetup();
    click('Continue');
    await flush();
    expect(postMock).not.toHaveBeenCalled();
    expect(screen.getByRole('heading', { level: 1, name: 'What your Chief of Staff will read' })).toBeInTheDocument();
  });

  it('uploads the logo as multipart field logo and shows it in the mark', async () => {
    serve({ ...goSetup, step: 'org', org: { name: 'Plainsong', has_logo: false, owner_name: '' } });
    formMock.mockResolvedValue({ ...goSetup, org: { name: 'Plainsong', has_logo: true, owner_name: '' } });
    await renderSetup();
    const logo = new File(['png'], 'mark.png', { type: 'image/png' });
    upload([logo]);
    await flush();
    expect(formMock.mock.calls[0]?.[0]).toBe('/api/v1/setup/org/logo');
    expect((formMock.mock.calls[0]?.[1] as FormData).get('logo')).toBe(logo);
    expect(within(screen.getByRole('img', { name: 'Plainsong logo' })).getByRole('presentation', { hidden: true })).toHaveAttribute('src', expect.stringContaining('/branding/icon-180.png'));
    expect(screen.queryByText('Drop a square logo')).toBeNull();
  });

  it('with a logo set the logo leads: Replace, no drop zone, and Replace opens the picker and uploads', async () => {
    serve({ ...goSetup, step: 'org', org: { name: 'Plainsong', has_logo: true, owner_name: '' } });
    const next = new File(['png'], 'next.png', { type: 'image/png' });
    vi.mocked(pickFiles).mockResolvedValue([next]);
    formMock.mockResolvedValue({ ...goSetup, org: { name: 'Plainsong', has_logo: true, owner_name: '' } });
    await renderSetup();
    expect(screen.getByRole('img', { name: 'Plainsong logo' })).toBeInTheDocument();
    expect(screen.queryByText('Drop a square logo')).toBeNull();
    expect(document.querySelector('.kv-drop')).toBeNull();
    click('Replace');
    await flush();
    expect(pickFiles).toHaveBeenCalledWith({ multiple: false, accept: 'image/png' });
    expect(formMock.mock.calls[0]?.[0]).toBe('/api/v1/setup/org/logo');
    expect((formMock.mock.calls[0]?.[1] as FormData).get('logo')).toBe(next);
  });

  it('dropping an image on the logo tile uploads it', async () => {
    serve({ ...goSetup, step: 'org', org: { name: 'Plainsong', has_logo: true, owner_name: '' } });
    const dropped = new File(['png'], 'drop.png', { type: 'image/png' });
    formMock.mockResolvedValue({ ...goSetup, org: { name: 'Plainsong', has_logo: true, owner_name: '' } });
    await renderSetup();
    fireEvent.drop(screen.getByTestId('logo-tile'), { dataTransfer: { files: [dropped] } });
    await flush();
    expect((formMock.mock.calls[0]?.[1] as FormData).get('logo')).toBe(dropped);
  });

  it('shows the server sentence when the logo is refused', async () => {
    serve({ ...goSetup, step: 'org', org: { name: 'Plainsong', has_logo: false, owner_name: '' } });
    formMock.mockRejectedValue(new ApiError(400, 'The logo must be square.', 'You can upload a square PNG.'));
    await renderSetup();
    upload([new File(['png'], 'wide.png', { type: 'image/png' })]);
    await flush();
    expect(screen.getByText('The logo must be square.')).toBeInTheDocument();
    expect(screen.getByText('Drop a square logo')).toBeInTheDocument();
  });

  it('shows what went wrong and who can fix it when the name is refused', async () => {
    serve({ ...goSetup, step: 'org', org: { name: '', has_logo: false, owner_name: '' } });
    postMock.mockRejectedValue(new ApiError(400, 'That name is too long.', 'You can shorten it.'));
    await renderSetup();
    fireEvent.change(screen.getByLabelText('Org name'), { target: { value: 'x' } });
    click('Continue');
    await flush();
    expect(screen.getByText('That name is too long.')).toBeInTheDocument();
    expect(screen.getByText('You can shorten it.')).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Your org' })).toBeInTheDocument();
  });
});

describe('Project files', () => {
  it('lists files with size and extraction status, uploads, and removes', async () => {
    serve({ ...goSetup, step: 'files' });
    const after: SetupData = { ...goSetup, files: [...goSetup.files, { sha: 'abc', name: 'pricing.csv', size_bytes: 2048, extracted: false }] };
    formMock.mockResolvedValue(after);
    await renderSetup();
    expect(screen.getByText('business-plan.pdf')).toBeInTheDocument();
    expect(screen.getByText('471 KB · Read')).toBeInTheDocument();
    expect(screen.getByText('1.1 MB · Still being read')).toBeInTheDocument();

    const a = new File(['a'], 'pricing.csv');
    const b = new File(['b'], 'deck.pdf');
    upload([a, b]);
    await flush();
    const form = formMock.mock.calls[0]?.[1] as FormData;
    expect(formMock.mock.calls[0]?.[0]).toBe('/api/v1/setup/files');
    expect(form.getAll('files[]')).toEqual([a, b]);
    expect(screen.getByText('pricing.csv')).toBeInTheDocument();

    deleteMock.mockResolvedValue({ ...goSetup, files: [goSetup.files[1]!] });
    click('Remove business-plan.pdf');
    await flush();
    expect(deleteMock).toHaveBeenCalledWith('/api/v1/setup/files/' + goSetup.files[0]!.sha);
    expect(screen.queryByText('business-plan.pdf')).toBeNull();
  });

  it('asks about the handbook only when there are files, checked by default', async () => {
    serve({ ...goSetup, step: 'files' });
    await renderSetup();
    expect(screen.getByRole('checkbox', { name: /Write the handbook from these files\?/ })).toBeChecked();
  });

  it('has no handbook question without files, and Skip moves on', async () => {
    serve({ ...ready, step: 'files', files: [] });
    await renderSetup();
    expect(screen.queryByRole('checkbox')).toBeNull();
    click('Skip');
    expect(screen.getByRole('heading', { level: 1, name: 'Hire your Chief of Staff' })).toBeInTheDocument();
  });
});

describe('Chief of Staff', () => {
  it('says it cannot start yet, in the server’s words, when no model is connected', async () => {
    serve({ ...goSetup, step: 'cos' });
    await renderSetup();
    expect(screen.getByRole('heading', { level: 1, name: 'Your Chief of Staff can’t start yet' })).toBeInTheDocument();
    expect(screen.getByText('No model is connected yet')).toBeInTheDocument();
    expect(screen.getByText(goSetup.credential.guidance, { exact: false })).toBeInTheDocument();
    expect(screen.getByText(/Everything you entered here is saved/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Hire Chief of Staff' })).toBeNull();
    expect(screen.getByText('Step 4 of 4')).toBeInTheDocument();
  });

  it('checks again on Try again and moves on once a model is connected', async () => {
    serve({ ...goSetup, step: 'cos' });
    await renderSetup();
    serve({ ...ready, step: 'cos' });
    click('Try again');
    await flush();
    expect(screen.getByRole('heading', { level: 1, name: 'Hire your Chief of Staff' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Hire Chief of Staff' })).toBeEnabled();
    expect(screen.queryByText('No model is connected yet')).toBeNull();
  });

  it('keeps the edited documents across Back and Continue', async () => {
    serve({ ...ready, step: 'cos' });
    postMock.mockResolvedValue({});
    await renderSetup();
    click('Edit role');
    fireEvent.change(screen.getByLabelText('Role document'), { target: { value: 'Mine.' } });
    click('Done editing');
    click('Back');
    click('Continue');
    click('Hire Chief of Staff');
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/setup/seed-cos', { handbook_from_files: true, role_md: 'Mine.' });
  });

  it('says it will propose a handbook from the files only when asked to', async () => {
    serve({ ...ready, step: 'files' });
    await renderSetup();
    click('Continue');
    expect(screen.getByText(/once it has read your files it proposes a better handbook/)).toBeInTheDocument();
    click('Back');
    fireEvent.click(screen.getByRole('checkbox', { name: /Write the handbook from these files\?/ }));
    click('Continue');
    expect(screen.queryByText(/proposes a better handbook/)).toBeNull();
  });

  it('shows the defaults read-only and closed', async () => {
    serve({ ...ready, step: 'cos' });
    await renderSetup();
    const details = document.querySelectorAll('details');
    expect(details).toHaveLength(2);
    for (const d of details) expect(d.open).toBe(false);
    expect(screen.getByRole('button', { name: 'Edit role' })).toBeInTheDocument();
    expect(screen.queryByRole('textbox')).toBeNull();
  });

  it('posts only the flag when nothing was edited', async () => {
    serve({ ...ready, step: 'files' });
    postMock.mockResolvedValue({});
    await renderSetup();
    click('Continue');
    click('Hire Chief of Staff');
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/setup/seed-cos', { handbook_from_files: true });
  });

  it('posts a false flag when unchecked, and the edited documents only', async () => {
    serve({ ...ready, step: 'files' });
    postMock.mockResolvedValue({});
    await renderSetup();
    fireEvent.click(screen.getByRole('checkbox', { name: /Write the handbook from these files\?/ }));
    click('Continue');
    click('Edit role');
    fireEvent.change(screen.getByLabelText('Role document'), { target: { value: '# Chief of Staff\n\nEdited.\n' } });
    click('Done editing');
    click('Hire Chief of Staff');
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/setup/seed-cos', { handbook_from_files: false, role_md: '# Chief of Staff\n\nEdited.\n' });
  });

  it('does not send a document that was edited back to its default', async () => {
    serve({ ...ready, step: 'cos' });
    postMock.mockResolvedValue({});
    await renderSetup();
    click('Edit handbook');
    const field = screen.getByLabelText('Handbook document');
    fireEvent.change(field, { target: { value: 'changed' } });
    fireEvent.change(field, { target: { value: ready.cos.default_handbook_md } });
    click('Hire Chief of Staff');
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/setup/seed-cos', { handbook_from_files: true });
  });

  it('shows the server error when the hire is refused', async () => {
    serve({ ...ready, step: 'cos' });
    postMock.mockRejectedValue(new ApiError(400, 'The role document is empty.', 'You can write one or use the default.'));
    await renderSetup();
    click('Hire Chief of Staff');
    await flush();
    expect(screen.getByText('The role document is empty.')).toBeInTheDocument();
    expect(screen.getByText('You can write one or use the default.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Hire Chief of Staff' })).toBeEnabled();
  });

  it('follows a hire already running instead of erroring on a 409', async () => {
    serve({ ...ready, step: 'cos' });
    postMock.mockRejectedValue(new ApiError(409, 'your Chief of Staff is already being hired', 'no one; it is still running'));
    await renderSetup();
    serve({ ...ready, step: 'cos' }, [goProgress]);
    click('Hire Chief of Staff');
    await flush();
    expect(screen.queryByText('your Chief of Staff is already being hired')).toBeNull();
    expect(screen.getByText('Hiring your Chief of Staff')).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Working' })).toBeInTheDocument();
  });

  it('lands on Done when a 409 means it is already hired', async () => {
    serve({ ...ready, step: 'cos' });
    postMock.mockRejectedValue(new ApiError(409, 'your Chief of Staff is already hired', 'no one'));
    await renderSetup();
    serve({ ...ready, step: 'cos' }, [{ state: 'done', stages: [], elapsed_s: 0 }]);
    click('Hire Chief of Staff');
    await flush();
    expect(screen.getByRole('heading', { level: 1, name: 'Your Chief of Staff is hired' })).toBeInTheDocument();
  });
});

describe('Hiring', () => {
  it('polls progress every second, renders the stages, and lands on Done', async () => {
    serve({ ...ready, step: 'cos' }, [goProgress, { ...goProgress, elapsed_s: 40 }, { ...goProgress, state: 'done', elapsed_s: 50 }]);
    // Resume shows no panel until the hire starts: the first /progress answer above is the resume probe.
    postMock.mockResolvedValue({});
    await renderSetup();
    // The probe found a running hire, so the panel is up and polling has begun.
    const progressCalls = () => getMock.mock.calls.filter(([p]) => p === '/api/v1/setup/progress').length;
    expect(screen.getByText('Reading your files')).toBeInTheDocument();
    expect(screen.getByText('Writing its first briefing')).toBeInTheDocument();
    expect(screen.getByText('0:34')).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Done' })).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Working' })).toBeInTheDocument();
    expect(screen.getAllByRole('status', { name: 'Queued' })).toHaveLength(2);
    const before = progressCalls();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(999);
    });
    expect(progressCalls()).toBe(before);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(progressCalls()).toBe(before + 1);
    expect(screen.getByText('0:40')).toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(screen.getByRole('heading', { level: 1, name: 'Your Chief of Staff is hired' })).toBeInTheDocument();
    const after = progressCalls();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(progressCalls()).toBe(after);
  });

  it('starts polling right after the hire is accepted', async () => {
    serve({ ...ready, step: 'cos' });
    postMock.mockResolvedValue({});
    await renderSetup();
    // No progress yet: the hire has not started.
    serve({ ...ready, step: 'cos' }, [goProgress]);
    click('Hire Chief of Staff');
    await flush();
    expect(screen.getByText('Hiring your Chief of Staff')).toBeInTheDocument();
    expect(screen.getByText('Writing its first briefing')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Hire Chief of Staff' })).toBeNull();
  });

  it('shows the failure with who can fix it, stops polling, and tries again', async () => {
    serve({ ...ready, step: 'cos' });
    postMock.mockResolvedValue({});
    await renderSetup();
    serve({ ...ready, step: 'cos' }, [goProgressFailed]);
    click('Hire Chief of Staff');
    await flush();
    expect(screen.getByText(goProgressFailed.error!)).toBeInTheDocument();
    expect(screen.getByText(/Who can fix it: you, by trying again/)).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Needs help' })).toBeInTheDocument();
    const calls = () => getMock.mock.calls.filter(([p]) => p === '/api/v1/setup/progress').length;
    const n = calls();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(calls()).toBe(n);

    serve({ ...ready, step: 'cos' }, [goProgress]);
    click('Try again');
    await flush();
    expect(postMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByText(goProgressFailed.error!)).toBeNull();
    expect(screen.getByRole('status', { name: 'Working' })).toBeInTheDocument();
  });

  it('names the hiring bar and stops polling when the page goes away', async () => {
    serve({ ...ready, step: 'cos' }, [goProgress]);
    const router = await renderSetup();
    expect(screen.getByRole('progressbar', { name: 'Hiring your Chief of Staff' })).toBeInTheDocument();
    const calls = () => getMock.mock.calls.filter(([p]) => p === '/api/v1/setup/progress').length;
    await act(async () => {
      await router.navigate('/');
    });
    const n = calls();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(calls()).toBe(n);
  });

  it('keeps asking after a missed progress check', async () => {
    serve({ ...ready, step: 'cos' }, [goProgress]);
    await renderSetup();
    const calls = () => getMock.mock.calls.filter(([p]) => p === '/api/v1/setup/progress').length;
    const n = calls();
    getMock.mockRejectedValueOnce(new ApiError(502, 'bad gateway', 'nobody'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(calls()).toBeGreaterThanOrEqual(n + 2);
    expect(screen.getByText('Hiring your Chief of Staff')).toBeInTheDocument();
  });

  it('goes back to the step from a failure', async () => {
    serve({ ...ready, step: 'cos' }, [goProgressFailed]);
    await renderSetup();
    click('Back');
    expect(screen.getByRole('heading', { level: 1, name: 'Hire your Chief of Staff' })).toBeInTheDocument();
  });
});

describe('Done', () => {
  it('says the handbook proposal is coming and goes to Home', async () => {
    serve({ ...goSetup, step: 'done' }, [{ ...goProgress, state: 'done' }]);
    const router = await renderSetup();
    expect(screen.getByText(/It read your 2 files and is drafting a handbook\. The proposal will be the first thing in Needs you\./)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open its chat' })).toHaveAttribute('href', '/agents/chief-of-staff');
    expect(screen.getAllByRole('button').filter((b) => b.className.includes('kv-btn--primary'))).toHaveLength(1);
    click('Go to Home');
    await flush();
    expect(router.state.location.pathname).toBe('/');
    expect(screen.getByText('Home page')).toBeInTheDocument();
  });

  it('does not promise a handbook without files', async () => {
    serve({ ...goSetup, step: 'done', files: [] }, [{ ...goProgress, state: 'done' }]);
    await renderSetup();
    expect(screen.queryByText(/drafting a handbook/)).toBeNull();
    expect(screen.getByText(/It will introduce itself in Home/)).toBeInTheDocument();
  });
});

describe('Pre-seeded name', () => {
  it('starts at Project files when the desktop app already named the team, and Back still reaches Your org', async () => {
    // The server's step for a stored name and no files.
    serve({ ...ready, step: 'files', files: [], org: { ...ready.org, name: 'Hearth' } });
    await renderSetup();
    expect(screen.getByRole('heading', { level: 1, name: 'What your Chief of Staff will read' })).toBeInTheDocument();
    expect(screen.getByText('Step 3 of 4')).toBeInTheDocument();
    click('Back');
    expect(screen.getByRole('textbox', { name: /Org name/ })).toHaveValue('Hearth');
  });
});

describe('Personal team', () => {
  const personal: SetupData = { ...ready, kind: 'personal' };

  it('reads the files as optional, asks nothing about the handbook, and moves on with none', async () => {
    serve({ ...personal, step: 'files', files: [] });
    await renderSetup();
    expect(screen.getByRole('heading', { level: 1, name: 'Anything your Chief of Staff should read' })).toBeInTheDocument();
    expect(screen.getByText(/Add files if you’d like.*Your Chief of Staff will ask you a few questions either way\./)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Skip' })).toBeNull();
    click('Continue');
    expect(screen.getByRole('heading', { level: 1, name: 'Hire your Chief of Staff' })).toBeInTheDocument();
  });

  it('has no handbook question with files either', async () => {
    serve({ ...personal, step: 'files' });
    await renderSetup();
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(screen.queryByText(/explains the business/)).toBeNull();
    expect(screen.getByText('business-plan.pdf')).toBeInTheDocument();
  });

  it('says it will ask a few questions, and posts a false flag', async () => {
    serve({ ...personal, step: 'cos' });
    postMock.mockResolvedValue({});
    await renderSetup();
    expect(screen.getByText(/it asks you a few questions first, then proposes the About you section/)).toBeInTheDocument();
    expect(screen.getByText('Runs your team day to day · reports to you')).toBeInTheDocument();
    click('Hire Chief of Staff');
    await flush();
    expect(postMock).toHaveBeenCalledWith('/api/v1/setup/seed-cos', { handbook_from_files: false });
  });

  it('says on Done that the questions come first, with or without files', async () => {
    serve({ ...personal, step: 'done' }, [{ ...goProgress, state: 'done' }]);
    await renderSetup();
    expect(screen.getByText(/It will send you a few questions in Needs you, then propose an About you section/)).toBeInTheDocument();
    expect(screen.queryByText(/drafting a handbook|company/)).toBeNull();
  });
});
