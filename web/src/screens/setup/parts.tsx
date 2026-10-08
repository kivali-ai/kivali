import type { ReactNode } from 'react';
import { ApiError } from '../../api/client';
import { Banner, Text } from '../../ds';

/** What a failed call says: what happened, then who can fix it. */
export function ErrorBanner({ error }: { error: unknown }) {
  if (error instanceof ApiError) {
    return (
      <Banner tone="danger" title={error.message}>
        {error.who}
      </Banner>
    );
  }
  return (
    <Banner tone="danger" title="That did not work.">
      Try again in a moment. If it keeps happening, whoever runs this Kivali server can look into it.
    </Banner>
  );
}

/** The step's h1 and its one line of framing. */
export function StepHead({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="app-setup-head">
      <Text as="h1" variant="title">
        {title}
      </Text>
      {children && (
        <Text as="p" tone="muted">
          {children}
        </Text>
      )}
    </div>
  );
}

/** Back, an optional link, Skip, then the primary, right-aligned. */
export function Actions({ children }: { children: ReactNode }) {
  return <div className="app-setup-actions">{children}</div>;
}
