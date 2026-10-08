import type { ComponentProps, ReactElement, ReactNode } from 'react';
import * as RTooltip from '@radix-ui/react-tooltip';

/**
 * Mount once near the root of the app (and around any isolated render in tests). Every `Tooltip` needs it.
 * Defaults to the reference's 300ms hover delay.
 */
export function TooltipProvider({ delayDuration = 300, ...rest }: ComponentProps<typeof RTooltip.Provider>) {
  return <RTooltip.Provider delayDuration={delayDuration} {...rest} />;
}

export interface TooltipProps {
  content: ReactNode;
  side?: 'top' | 'right' | 'bottom' | 'left';
  open?: boolean;
  children: ReactElement;
}

/**
 * A short label that appears on hover or focus, mainly to name icon-only buttons. Built on Radix Tooltip; requires a `TooltipProvider` above it.
 *
 * - `content` is a few words in sentence case, no period. `side` places it; it flips when there is no room.
 * - Tooltips repeat or clarify; they never hold information found nowhere else, because touch screens cannot hover.
 * - Ink fill with on-ink text, in both themes.
 */
export function Tooltip({ content, children, side = 'top', open }: TooltipProps) {
  return (
    <RTooltip.Root open={open}>
      <RTooltip.Trigger asChild>{children}</RTooltip.Trigger>
      <RTooltip.Portal>
        <RTooltip.Content className="kv-tooltip" side={side} sideOffset={6}>
          {content}
        </RTooltip.Content>
      </RTooltip.Portal>
    </RTooltip.Root>
  );
}
