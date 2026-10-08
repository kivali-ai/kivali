import type { HTMLAttributes, ReactNode } from 'react';
import { cx } from '../cx';

export type TextElement = 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6' | 'p' | 'span' | 'div' | 'label' | 'dt' | 'dd';
export type TextVariant = 'display' | 'title' | 'heading' | 'body' | 'caption' | 'label' | 'code';
export type TextTone = 'default' | 'muted' | 'faint' | 'signal' | 'cobalt' | 'success' | 'danger';

export interface TextProps extends HTMLAttributes<HTMLElement> {
  /** The element to render. Default `span`. Pick it for meaning (h1–h6 for headings), and `variant` for the look. */
  as?: TextElement;
  /** A step of the type scale. Default `body`. `label` and `code` are set in the mono face. */
  variant?: TextVariant;
  /** Text colour. Omit it to inherit the colour of the surrounding surface. */
  tone?: TextTone;
  /** For `as="label"`. */
  htmlFor?: string;
  children?: ReactNode;
}

/**
 * Text in the Kivali type scale. A Kivali addition: the vendored system has type tokens but no type classes.
 *
 * - `variant`: `display` (the one big number or name on a page), `title` (page title), `heading` (section),
 *   `body` (default), `caption` (secondary lines), `label` (mono, small metadata), `code` (mono).
 * - `tone`: `default` (ink), `muted`, `faint`, `signal` (needs the person), `cobalt`, `success`, `danger`.
 * - Margins are zeroed; spacing belongs to the layout.
 */
export function Text({ as: Tag = 'span', variant = 'body', tone, className, children, ...rest }: TextProps) {
  return (
    <Tag className={cx('kv-text', 'kv-text-' + variant, tone && 'kv-text--' + tone, className)} {...rest}>
      {children}
    </Tag>
  );
}
