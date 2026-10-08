// Builds the binaries when missing and starts the shared server: the demo seed, read by every
// spec that does not change data (a11y, visual, most screen checks). A spec that writes gets a
// fresh server of its own through the `kivali` fixture, so the shared one stays as seeded.
import { ensureBinaries, startServer } from './support/server';

export default async function globalSetup(): Promise<() => Promise<void>> {
  ensureBinaries();
  const shared = await startServer({ seed: 'demo' });
  process.env.KIVALI_SHARED_URL = shared.baseURL;
  process.env.KIVALI_SHARED_LOG = shared.logPath;
  return async () => {
    await shared.stop();
  };
}
