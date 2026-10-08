import { defineConfig } from '@playwright/test';
import type { Project } from '@playwright/test';

// Visual baselines are platform-specific (fonts and anti-aliasing differ between macOS and
// the Linux CI image), so the visual spec runs only when asked for. See e2e/README.md.
const visual = process.env.KIVALI_VISUAL === '1';

const desktop = { viewport: { width: 1440, height: 900 } };
const phone = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };

const projects: Project[] = [
  {
    name: 'desktop',
    use: { ...desktop, colorScheme: 'light' },
    testIgnore: visual ? [] : ['visual.spec.ts'],
  },
  {
    name: 'phone',
    use: { ...phone, colorScheme: 'light' },
    testIgnore: visual ? [] : ['visual.spec.ts'],
  },
];
if (visual) {
  projects.push(
    { name: 'desktop-dark', use: { ...desktop, colorScheme: 'dark' }, testMatch: ['visual.spec.ts'] },
    { name: 'phone-dark', use: { ...phone, colorScheme: 'dark' }, testMatch: ['visual.spec.ts'] },
  );
}

export default defineConfig({
  testDir: './specs',
  outputDir: './test-results',
  snapshotPathTemplate: '{testDir}/../__screenshots__/{platform}/{projectName}/{arg}{ext}',
  globalSetup: './global-setup.ts',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0,
  // Every test that changes data gets its own server; a handful at once is plenty.
  workers: process.env.CI ? 2 : 4,
  timeout: 60_000,
  expect: {
    timeout: 10_000,
    toHaveScreenshot: { animations: 'disabled', caret: 'hide', scale: 'css' },
  },
  reporter: process.env.CI ? [['list'], ['html', { outputFolder: 'playwright-report', open: 'never' }]] : 'list',
  use: {
    // baseURL comes from the `kivali` fixture (support/fixtures.ts): the shared server the
    // global setup started, or a fresh one for a test that changes data.
    browserName: 'chromium',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    locale: 'en-US',
    timezoneId: 'UTC',
  },
  projects,
});
