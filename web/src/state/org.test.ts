import { describe, expect, it } from 'vitest';
import { autoReleaseLabel, autoReleaseValue, buildTree, flattenTree, initialOrgState, reduceSnapshot, withAutoRelease } from './org';
import { busySnapshot, goSnapshot, quietSnapshot, releaseOnlySnapshot } from './fixtures';

describe('reduceSnapshot', () => {
  it('reads the Go golden snapshot', () => {
    const s = reduceSnapshot(initialOrgState, goSnapshot);
    expect(s.ready).toBe(true);
    expect(s.needs).toBe(1);
    expect(s.autoRelease).toBe('30s');
    expect(s.running).toBe(true);
    expect(s.assignmentsVersion).toBe(17);
    expect(s.goals).toHaveLength(2);
    expect(s.tree.map((n) => n.slug)).toEqual(['chief-of-staff']);
    // The count behind "Waiting on N tasks"; 0 when the snapshot omits it.
    expect(s.tree[0]?.waitingTasks).toBe(2);
    const vp = s.tree[0]?.children[0];
    expect(vp?.waitingTasks).toBe(0);
    expect(vp?.slug).toBe('engineering-lead');
    expect(vp?.contextPct).toBe(82);
    expect(vp?.children.map((c) => c.slug)).toEqual(['buyer', 'tester']);
  });

  it('builds the tree by reporting line and keeps the server depth', () => {
    const tree = buildTree(busySnapshot.agents);
    expect(tree.map((n) => n.slug)).toEqual(['chief-of-staff', 'advisor']);
    expect(flattenTree(tree).map((n) => [n.slug, n.depth])).toEqual([
      ['chief-of-staff', 1],
      ['engineering-lead', 2],
      ['test-runner', 3],
      ['support-lead', 2],
      ['advisor', 1],
    ]);
  });

  it('keeps an agent whose manager is missing at the top instead of dropping it', () => {
    const tree = buildTree([{ ...quietSnapshot.agents[1]!, reports_to: 'gone' }]);
    expect(tree.map((n) => n.slug)).toEqual(['bookkeeper']);
  });

  it('counts the needs and reports running from any agent', () => {
    const s = reduceSnapshot(initialOrgState, busySnapshot);
    expect(s.needs).toBe(3);
    expect(s.running).toBe(true);
    expect(s.autoRelease).toBe('5m');
  });

  it('is quiet when nothing runs and nothing waits', () => {
    const s = reduceSnapshot(initialOrgState, quietSnapshot);
    expect(s.needs).toBe(0);
    expect(s.running).toBe(false);
  });

  it('counts the release engine as running even with every agent idle', () => {
    const s = reduceSnapshot(initialOrgState, releaseOnlySnapshot);
    expect(s.running).toBe(true);
    expect(s.autoRelease).toBe('off');
  });

  it('is pure: the previous state is untouched and a later snapshot replaces the tree', () => {
    const first = reduceSnapshot(initialOrgState, busySnapshot);
    const before = JSON.stringify(first);
    const second = reduceSnapshot(first, quietSnapshot);
    expect(JSON.stringify(first)).toBe(before);
    expect(second.tree.map((n) => n.slug)).toEqual(['chief-of-staff']);
    expect(initialOrgState.ready).toBe(false);
    // Off until the first snapshot says otherwise, as on the server.
    expect(initialOrgState.autoRelease).toBe('off');
  });
});

describe('auto-release helpers', () => {
  it('maps wire values to the stop labels and back', () => {
    expect(autoReleaseLabel('now')).toBe('Now');
    expect(autoReleaseLabel('off')).toBe('Off');
    expect(autoReleaseValue('Now')).toBe('now');
    expect(autoReleaseValue('20m')).toBe('20m');
    expect(autoReleaseValue('soon')).toBeUndefined();
  });

  it('withAutoRelease returns the same object when nothing changes', () => {
    const s = reduceSnapshot(initialOrgState, quietSnapshot);
    expect(withAutoRelease(s, '30s')).toBe(s);
    expect(withAutoRelease(s, '2m').autoRelease).toBe('2m');
  });
});
