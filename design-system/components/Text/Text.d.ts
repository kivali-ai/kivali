import * as React from 'react';
export type TextElement = 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6' | 'p' | 'span' | 'div' | 'label' | 'dt' | 'dd';
export type TextVariant = 'display' | 'title' | 'heading' | 'body' | 'caption' | 'label' | 'code';
export type TextTone = 'default' | 'muted' | 'faint' | 'signal' | 'cobalt' | 'success' | 'danger';
/**
 * Props for Text.
 */
export interface TextProps extends React.HTMLAttributes<HTMLElement> {
  /** The element to render. Default `span`. Pick it for meaning; `variant` sets the look. */
  as?: TextElement;
  /** A step of the type scale. Default `body`. `label` and `code` are set in the mono face. */
  variant?: TextVariant;
  /** Text colour. Omit to inherit from the surrounding surface. */
  tone?: TextTone;
  /** For `as="label"`. */
  htmlFor?: string;
  className?: string;
  children?: React.ReactNode;
}
export declare function Text(props: TextProps): JSX.Element;
