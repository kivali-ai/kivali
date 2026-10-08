import * as React from 'react';
/**
 * Props for Card.
 * @startingPoint section="Layout" subtitle="Raised card with title, meta, actions" viewport="700x420"
 */
export interface CardProps {
  title?: React.ReactNode;
  meta?: React.ReactNode;
  actions?: React.ReactNode;
  collapsible?: boolean;
  defaultOpen?: boolean;
  tone?: 'attention';
  children?: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
}
export declare function Card(props: CardProps): JSX.Element;
