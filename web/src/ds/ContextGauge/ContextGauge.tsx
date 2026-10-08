import { Badge } from '../Badge/Badge';
import { Button } from '../Button/Button';
import { Icon } from '../Icon/Icon';
import { Progress } from '../Progress/Progress';

export interface ContextGaugeProps {
  value?: number;
  threshold?: number;
  onNewChat?(): void;
  compact?: boolean;
  title?: string;
}

const toneFor = (pct: number) => (pct < 50 ? 'success' : 'cobalt');

/**
 * Tells the person an agent's chat is nearly full, next to the place they can act on it.
 *
 * - Under the threshold (default 80) it is a quiet gauge: teal under 50%, cobalt up to 80%, the percentage in words.
 * - Past the threshold it becomes a solid signal badge "Context 82% full" and New chat turns primary. No banner over the transcript.
 * - Temporary: remove when chat rotation is automatic.
 */
export function ContextGauge({ value = 0, threshold = 80, onNewChat, compact = false, title }: ContextGaugeProps) {
  const pct = Math.round(value);
  const long = pct >= threshold;
  return (
    <span className="kv-ctx" title={title}>
      {long ? (
        <Badge tone="signal" variant="solid">
          Context {pct}% full
        </Badge>
      ) : (
        <span className="kv-ctx-gauge">
          <span className="kv-ctx-label">
            Context <b>{pct}%</b>
          </span>
          <span className="kv-ctx-bar">
            <Progress value={pct} tone={toneFor(pct)} aria-label={`Context ${pct}% full`} />
          </span>
        </span>
      )}
      {onNewChat && (
        <Button variant={long ? 'primary' : 'secondary'} size={compact ? 'sm' : 'md'} icon={<Icon name="rotate-ccw" />} onClick={onNewChat}>
          New chat
        </Button>
      )}
    </span>
  );
}

export interface ContextCountProps {
  value?: number;
  threshold?: number;
}

/**
 * For NavItem's count slot in the org tree and Team rows: a soft signal percentage, shown only past the threshold (renders nothing below it).
 */
export function ContextCount({ value = 0, threshold = 80 }: ContextCountProps) {
  const pct = Math.round(value);
  if (pct < threshold) return null;
  return (
    <span title={'Context ' + pct + '% full · start a new chat'}>
      <Badge tone="signal" mono>
        {pct}%
      </Badge>
    </span>
  );
}
