import type { HTMLAttributes, ReactNode } from 'react';
import { cx } from '../cx';

export type BadgeTone = 'neutral' | 'signal' | 'cobalt' | 'success' | 'danger';

export interface BadgeProps extends Omit<HTMLAttributes<HTMLSpanElement>, 'children'> {
  tone?: BadgeTone;
  variant?: 'soft' | 'solid' | 'outline';
  mono?: boolean;
  children: ReactNode;
}

/**
 * A small pill for a count, a kind, a model or a short status word.
 *
 * - `tone`: `neutral` (default), `signal` (needs the person), `cobalt` (learned, in progress), `success`, `danger`.
 * - `variant`: `soft` (default, tinted), `solid` (counts and the one thing to notice), `outline` (quiet metadata).
 * - `mono` sets it in the mono face, for models, IDs and readouts.
 * - Keep the text to one to three words. For an agent's run state use `AgentState`, not a badge.
 * - Use a badge when the status is why the item is in front of the person, or separates it from neighbours with mixed statuses. Settled history goes in plain text with the time; if the section already says it, say nothing.
 * - At most two badges per item: one status, plus one piece of metadata if it earns the space. Only one `signal` badge per card.
 * - Tones: needs the person is `signal`; in progress or learned is `cobalt`; finished well is `success`; failed or broken is `danger`; settled, neutral, denied, closed, cancelled, draft, archived is `neutral`. A decision the person made is never `danger`.
 */
export function Badge({ tone = 'neutral', variant = 'soft', mono = false, className, children, ...rest }: BadgeProps) {
  return (
    <span className={cx('kv-badge', 'kv-badge--' + tone, 'kv-badge--' + variant, mono && 'kv-badge--mono', className)} {...rest}>
      {children}
    </span>
  );
}
