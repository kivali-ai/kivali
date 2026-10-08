import type { ReactElement, ReactNode } from 'react';
import * as RDialog from '@radix-ui/react-dialog';
import { cx } from '../cx';
import { Icon } from '../Icon/Icon';

export interface DialogProps {
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?(open: boolean): void;
  trigger?: ReactElement;
  title: ReactNode;
  description?: ReactNode;
  footer?: ReactNode;
  tone?: 'danger';
  children?: ReactNode;
}

/**
 * A modal for a decision that needs full attention: confirming a destructive action, or a short form. Built on Radix Dialog (focus trap, Escape to close, labelled title).
 *
 * - `title` asks the question ("Offboard Garden advisor?"); `description` says what will happen; `footer` holds the buttons, cancel (`secondary`) first and the action last.
 * - `tone="danger"` colors the title for destructive confirmations; the action button is then `danger`.
 * - Control it with `open` and `onOpenChange`, or give it a `trigger` element. Wrap cancel in `DialogClose`.
 * - Replace every browser `confirm()` with this.
 */
export function Dialog({ open, defaultOpen, onOpenChange, trigger, title, description, footer, tone, children }: DialogProps) {
  return (
    <RDialog.Root open={open} defaultOpen={defaultOpen} onOpenChange={onOpenChange}>
      {trigger && <RDialog.Trigger asChild>{trigger}</RDialog.Trigger>}
      <RDialog.Portal>
        <RDialog.Overlay className="kv-dialog-overlay" />
        <RDialog.Content
          aria-modal="true"
          {...(description ? {} : { 'aria-describedby': undefined })}
          className={cx('kv-dialog', tone && 'kv-dialog--' + tone)}
        >
          <div className="kv-dialog-head">
            <RDialog.Title className="kv-dialog-title">{title}</RDialog.Title>
            <RDialog.Close className="kv-dialog-close" aria-label="Close">
              <Icon name="x" />
            </RDialog.Close>
          </div>
          {description && <RDialog.Description className="kv-dialog-desc">{description}</RDialog.Description>}
          {children && <div className="kv-dialog-body">{children}</div>}
          {footer && <div className="kv-dialog-foot">{footer}</div>}
        </RDialog.Content>
      </RDialog.Portal>
    </RDialog.Root>
  );
}

/**
 * Wraps a button so clicking it closes the enclosing Dialog: `<DialogClose asChild><Button>Cancel</Button></DialogClose>`.
 */
export function DialogClose({ children }: { asChild?: boolean; children: ReactElement }) {
  return <RDialog.Close asChild>{children}</RDialog.Close>;
}
