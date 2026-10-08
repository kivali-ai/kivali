import type { ReactNode } from 'react';
import { Text } from '../ds';

/**
 * A screen's title on desktop. Below 960 the frame's phone header carries the title, so this hides there and
 * there is one visible h1 at every width.
 */
export function PageTitle({ children }: { children: ReactNode }) {
  return (
    <Text as="h1" variant="title" className="app-page-title">
      {children}
    </Text>
  );
}
