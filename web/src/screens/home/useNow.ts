import { useEffect, useState } from 'react';

export type Clock = () => number;

export const systemClock: Clock = () => Date.now();

/**
 * The current time from `clock`, re-read every `intervalMs`. The interval restarts when the period changes
 * and is cleared on unmount. Home ticks once a second while a countdown runs and once a minute otherwise
 * (for "8m" style times). Tests drive it with fake timers.
 */
export function useNow(intervalMs: number, clock: Clock = systemClock): number {
  const [now, setNow] = useState(clock);
  useEffect(() => {
    setNow(clock());
    const id = setInterval(() => setNow(clock()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs, clock]);
  return now;
}
