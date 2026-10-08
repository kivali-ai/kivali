import type { CSSProperties, ReactNode } from 'react';
import { cx } from '../cx';

export interface ProseProps {
  html?: string;
  children?: ReactNode;
  className?: string;
  style?: CSSProperties;
}

/**
 * Styling for rendered markdown: agent roles, memory, the handbook, assignment descriptions, docs.
 *
 * - Wrap rendered HTML with `html`, or pass elements as children. Text sets in body at a comfortable measure (about 68 characters).
 * - Headings use the display face, links are cobalt and underlined, inline code sits on a line tint, blockquotes get a thin ink-faint rule, and fenced code gets the `CodeBlock` look.
 * - Sanitize any agent- or user-written markdown before passing it as `html`.
 */
export function Prose({ children, html, className, style }: ProseProps) {
  return html != null ? (
    <div className={cx('kv-prose', className)} style={style} dangerouslySetInnerHTML={{ __html: html }} />
  ) : (
    <div className={cx('kv-prose', className)} style={style}>
      {children}
    </div>
  );
}
