import { describe, expect, it } from 'vitest';
import { runStateView } from './runState';

describe('runStateView', () => {
  it('says an agent waiting with background tasks is waiting on them', () => {
    expect(runStateView('waiting', 2)).toEqual({ state: 'queued', label: 'Waiting on tasks' });
  });

  it('says an agent waiting with no tasks was stopped by the person (the held state)', () => {
    expect(runStateView('waiting', 0)).toEqual({ state: 'queued', label: 'Stopped by you' });
    expect(runStateView('waiting')).toEqual({ state: 'queued', label: 'Stopped by you' });
  });

  it('leaves the other states alone whatever the count', () => {
    expect(runStateView('running', 3)).toEqual({ state: 'running' });
    expect(runStateView('idle', 0)).toBeNull();
  });
});
