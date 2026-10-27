import { act, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { FakeStream, stubFetch } from '../app/test-utils';
import { goMe, goSnapshot } from './fixtures';
import { OrgProvider, useOrg } from './OrgProvider';

function Probe() {
  const { org } = useOrg();
  return <div data-testid="total">{org.ready ? String(org.messagesTotal) : 'loading'}</div>;
}

function renderProvider() {
  const stream = new FakeStream();
  const reload = vi.fn();
  stubFetch({ 'GET /api/v1/me': goMe, 'GET /api/v1/snapshot': goSnapshot });
  render(
    <OrgProvider createStream={() => stream} reload={reload}>
      <Probe />
    </OrgProvider>,
  );
  return { stream, reload };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

// Guards the reload on a changed server build. A Kivali update keeps the page's tab (Kivali Desktop hides it
// behind a progress screen and shows it again), so the old app would otherwise go on reading the new server
// until someone reloaded by hand; and a reload on every reconnect would throw away the page's state for nothing.
describe('OrgProvider and the server build', () => {
  it('applies snapshots from the build the page loaded under', async () => {
    const { stream, reload } = renderProvider();
    await waitFor(() => expect(screen.getByTestId('total')).toHaveTextContent(String(goSnapshot.messages_total)));
    act(() => stream.emit('snapshot', { ...goSnapshot, messages_total: goSnapshot.messages_total + 1 }));
    expect(screen.getByTestId('total')).toHaveTextContent(String(goSnapshot.messages_total + 1));
    expect(reload).not.toHaveBeenCalled();
    expect(stream.stopped).toBe(false);
  });

  it('reloads the page, once, when a snapshot names another build', async () => {
    const { stream, reload } = renderProvider();
    await waitFor(() => expect(screen.getByTestId('total')).toHaveTextContent(String(goSnapshot.messages_total)));
    act(() => stream.emit('snapshot', { ...goSnapshot, version: goSnapshot.version + '.next', messages_total: 999 }));
    expect(reload).toHaveBeenCalledTimes(1);
    // The new build's snapshot is not applied by the old app, and the stream is stopped before the reload.
    expect(screen.getByTestId('total')).toHaveTextContent(String(goSnapshot.messages_total));
    expect(stream.stopped).toBe(true);
  });
});
