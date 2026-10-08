// Starts and stops a real Kivali server for the end-to-end suite: the Go binary in DEV_MODE
// over a DATA_DIR that cmd/devseed built, plus one agent runtime per agent (the `agent`
// subcommand an agent pod runs), all with cmd/fake-claude on PATH as `claude`. Chat turns
// only run through an agent runtime, so without them nothing would ever answer.
//
// Readiness is event-driven: the server is up when it logs that it is listening (and
// /healthz answers), and the runtimes are up when the /org/stream snapshot stops listing
// their agents as disconnected. Nothing here polls on a timer.
import { spawn, spawnSync } from 'node:child_process';
import type { ChildProcess } from 'node:child_process';
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { join, resolve } from 'node:path';

export const repoRoot = resolve(import.meta.dirname, '../../..');
/** KIVALI_E2E_BIN_DIR points at Linux builds when the suite runs in the Playwright image (see README). */
export const binDir = process.env.KIVALI_E2E_BIN_DIR ? resolve(process.env.KIVALI_E2E_BIN_DIR) : join(repoRoot, 'bin');
export const serverBin = join(binDir, 'kivali-e2e');
export const devseedBin = join(binDir, 'devseed');
/** fake-claude is built under the name the runner looks up on PATH. */
export const fakeClaudeDir = join(binDir, 'fakebin');
export const fakeClaudeBin = join(fakeClaudeDir, 'claude');

export type Seed = 'demo' | 'empty' | 'setup';

/**
 * Agents that get a runtime whether or not they exist yet: the Chief of Staff so the setup
 * wizard's hire can answer, and every agent the demo seeds.
 */
const runtimeSlugs = ['chief-of-staff'];

export interface RunningServer {
  baseURL: string;
  dataDir: string;
  logPath: string;
  stop: () => Promise<void>;
}

function run(cmd: string, args: string[], cwd: string, env: NodeJS.ProcessEnv = process.env, quiet = false): void {
  const r = spawnSync(cmd, args, { cwd, env, stdio: quiet ? ['ignore', 'ignore', 'pipe'] : 'inherit' });
  if (r.status !== 0) {
    const detail = quiet ? `: ${r.stderr?.toString().trim()}` : '';
    throw new Error(`${cmd} ${args.join(' ')} failed with status ${r.status}${detail}`);
  }
}

/** The go toolchain misbehaves under a stale inherited GOROOT; let it find its own. */
function goEnv(): NodeJS.ProcessEnv {
  const env = { ...process.env };
  delete env.GOROOT;
  return env;
}

/**
 * Builds whatever is missing. `make web-e2e` always rebuilds all three first; this is for a
 * bare `npx playwright test`. The server binary embeds internal/web/ui/dist, so the web app
 * is built first when that is empty.
 */
export function ensureBinaries(): void {
  if (!existsSync(join(repoRoot, 'internal/web/ui/dist/index.html'))) {
    run('npm', ['run', 'build'], join(repoRoot, 'web'));
  }
  if (!existsSync(serverBin)) run('go', ['build', '-o', serverBin, '.'], repoRoot, goEnv());
  if (!existsSync(devseedBin)) run('go', ['build', '-o', devseedBin, './cmd/devseed'], repoRoot, goEnv());
  if (!existsSync(fakeClaudeBin)) run('go', ['build', '-o', fakeClaudeBin, './cmd/fake-claude'], repoRoot, goEnv());
}

async function freePort(): Promise<number> {
  return new Promise((res, rej) => {
    const srv = createServer();
    srv.unref();
    srv.on('error', rej);
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      if (addr === null || typeof addr === 'string') return rej(new Error('no port'));
      srv.close(() => res(addr.port));
    });
  });
}

/** Unix socket paths are capped near 104 bytes on macOS, so the scratch root stays short. */
function scratchRoot(): string {
  return mkdtempSync(join(process.platform === 'win32' ? process.env.TEMP ?? '.' : '/tmp', 'kv-'));
}

/** Resolves when `line` appears in the child's stderr; rejects if the child exits first. */
function waitForLog(child: ChildProcess, needle: string, what: string): Promise<void> {
  return new Promise((res, rej) => {
    let buf = '';
    const onData = (chunk: Buffer) => {
      buf += chunk.toString();
      if (buf.includes(needle)) {
        cleanup();
        res();
      }
      if (buf.length > 1 << 16) buf = buf.slice(-(1 << 15));
    };
    const onExit = (code: number | null) => {
      cleanup();
      rej(new Error(`${what} exited (${code}) before logging "${needle}"`));
    };
    const cleanup = () => {
      child.stderr?.off('data', onData);
      child.off('exit', onExit);
    };
    child.stderr?.on('data', onData);
    child.on('exit', onExit);
  });
}

interface SnapshotAgent {
  slug: string;
  state: string;
}

/** Reads /org/stream until a snapshot shows every slug in `slugs` connected. */
async function waitForAgentsConnected(baseURL: string, slugs: string[]): Promise<void> {
  if (slugs.length === 0) return;
  const ctrl = new AbortController();
  const res = await fetch(`${baseURL}/org/stream`, { signal: ctrl.signal });
  if (!res.ok || !res.body) throw new Error(`/org/stream answered ${res.status}`);
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = '';
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) throw new Error('/org/stream closed before the agents connected');
      buf += value;
      let cut: number;
      while ((cut = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, cut);
        buf = buf.slice(cut + 2);
        const data = frame
          .split('\n')
          .filter((l) => l.startsWith('data: '))
          .map((l) => l.slice(6))
          .join('');
        if (!frame.includes('event: snapshot') || data === '') continue;
        const snap = JSON.parse(data) as { agents?: SnapshotAgent[] };
        const byslug = new Map((snap.agents ?? []).map((a) => [a.slug, a.state]));
        if (slugs.every((s) => byslug.has(s) && byslug.get(s) !== 'disconnected')) return;
      }
    }
  } finally {
    ctrl.abort();
  }
}

function stopChild(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((res) => {
    child.once('exit', () => res());
    child.kill('SIGTERM');
  });
}

export interface StartOptions {
  seed: Seed;
  /** FAKE_CLAUDE_SCENARIO for every turn that does not tag its own. */
  scenario?: string;
  /** devseed's -now (RFC 3339): every seeded time is relative to it. Defaults to the current minute. */
  now?: string;
  /** FAKE_CLAUDE_PAUSE_MS: how long the `slow` scenario waits between deltas. Defaults to 20 seconds. */
  pauseMs?: number;
}

/** Seeds a fresh DATA_DIR, starts the server and the agent runtimes, and waits until all are ready. */
export async function startServer(opts: StartOptions): Promise<RunningServer> {
  ensureBinaries();
  const root = scratchRoot();
  const dataDir = join(root, 'data');
  const home = join(root, 'home');
  const uds = join(root, 'uds');
  const filesRoot = join(root, 'files');
  mkdirSync(home, { recursive: true });
  mkdirSync(filesRoot, { recursive: true });
  const seedArgs = ['-data', dataDir, '-scenario', opts.seed];
  if (opts.now) seedArgs.push('-now', opts.now);
  run(devseedBin, seedArgs, repoRoot, process.env, true);

  const port = await freePort();
  const baseURL = `http://127.0.0.1:${port}`;
  const env: NodeJS.ProcessEnv = {
    ...goEnv(),
    DEV_MODE: 'true',
    ADDR: `127.0.0.1:${port}`,
    DATA_DIR: dataDir,
    AGENTPOD_UDS_DIR: uds,
    HOME: home,
    PATH: `${fakeClaudeDir}:${process.env.PATH ?? ''}`,
    FAKE_CLAUDE_SCENARIO: opts.scenario ?? 'reply',
    // Pauses the `slow` scenario takes between deltas. Long enough by default that a Stop always
    // lands inside one; the interrupt ends the pause at once, so the suite never waits it out. A
    // test that lets a slow turn run to its end asks for a short one.
    FAKE_CLAUDE_PAUSE_MS: String(opts.pauseMs ?? 20000),
    // How long the `fold` scenario keeps a folded message queued before its tool seam takes
    // it, which is how long the page shows it pending.
    FAKE_CLAUDE_FOLD_HOLD_MS: '3000',
    // There is no dev-shell sidecar here: the MCP server runs the file tools in process, against
    // a local tree of the run's own (a host has no /files), or it stops at start and every tool
    // call the fake makes goes unanswered.
    KIVALI_MCP_LOCAL_FILES: '1',
    KIVALI_MCP_LOCAL_FILES_ROOT: filesRoot,
  };
  const logPath = join(root, 'server.log');
  const log = createWriteStream(logPath);
  // One pipe per process (the server and each runtime, stdout and stderr) share this stream.
  log.setMaxListeners(0);
  const children: ChildProcess[] = [];

  const server = spawn(serverBin, [], { cwd: root, env, stdio: ['ignore', 'pipe', 'pipe'] });
  children.push(server);
  server.stdout.pipe(log, { end: false });
  server.stderr.pipe(log, { end: false });
  const stop = async () => {
    for (const c of children.slice().reverse()) await stopChild(c);
    log.end();
    if (process.env.KIVALI_E2E_KEEP !== '1') rmSync(root, { recursive: true, force: true });
  };
  try {
    await waitForLog(server, 'kivali listening on', 'the server');
    const health = await healthz(baseURL);
    if (!health.ok) throw new Error(`/healthz answered ${health.status}`);

    const seeded = await listAgentSlugs(baseURL);
    const slugs = [...new Set([...runtimeSlugs, ...seeded])].sort();
    for (const slug of slugs) {
      const scratch = join(root, 'scratch', slug);
      mkdirSync(scratch, { recursive: true });
      const rt = spawn(serverBin, ['agent', '--slug', slug, '--uds', join(uds, 'core.sock'), '--scratch', scratch], {
        cwd: scratch,
        env,
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      children.push(rt);
      rt.stdout.pipe(log, { end: false });
      rt.stderr.pipe(log, { end: false });
    }
    await waitForAgentsConnected(baseURL, seeded);
  } catch (err) {
    await stop();
    throw new Error(`${(err as Error).message} (server log: ${logPath})`);
  }
  return { baseURL, dataDir, logPath, stop };
}

/**
 * GET /healthz. The server logs "listening" on the line before it binds the port, so on a slow
 * machine (the emulated Playwright image) the first connection can still be refused; that one
 * error, and only that one, is retried, a bounded number of times.
 */
async function healthz(baseURL: string): Promise<Response> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await fetch(`${baseURL}/healthz`);
    } catch (err) {
      const code = (err as { cause?: { code?: string } }).cause?.code;
      if (code !== 'ECONNREFUSED' || attempt >= 50) throw err;
      await new Promise((r) => setTimeout(r, 100));
    }
  }
}

/** Every active agent but the CEO, who is a person and has no runtime. */
async function listAgentSlugs(baseURL: string): Promise<string[]> {
  const res = await fetch(`${baseURL}/api/v1/agents`);
  if (!res.ok) throw new Error(`/api/v1/agents answered ${res.status}`);
  const body = (await res.json()) as { agents?: { slug: string }[] };
  return (body.agents ?? []).map((a) => a.slug).filter((s) => s !== 'ceo');
}
