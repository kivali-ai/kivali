import type { ReactNode } from 'react';
import type { ApiError } from '../../api/client';
import { Banner, Skeleton, Text } from '../../ds';

/** A section's heading row. Desktop only: on phone the frame header already names the sub-page. */
export function SectionHead({ title, meta, show }: { title: string; meta?: ReactNode; show: boolean }) {
  if (!show) return null;
  return (
    <div className="app-org-head">
      <Text as="h2" variant="heading" className="app-org-title">
        {title}
      </Text>
      {meta && (
        <Text variant="label" tone="muted">
          {meta}
        </Text>
      )}
    </div>
  );
}

/** What went wrong and who can fix it, in one Banner. */
export function ErrorBanner({ error, onDismiss }: { error: ApiError | null; onDismiss?: () => void }) {
  if (!error) return null;
  return (
    <Banner tone="danger" title={error.message} {...(onDismiss ? { onDismiss } : {})}>
      {error.who}
    </Banner>
  );
}

/** One raised container for a list of rows, hairline-divided. */
export function RowList({ children }: { children: ReactNode }) {
  return (
    <div className="app-org-list">
      <div className="app-org-rows">{children}</div>
    </div>
  );
}

export function RowSkeletons({ count = 2 }: { count?: number }) {
  return (
    <div className="app-org-list" aria-hidden="true">
      <div className="app-org-rows">
        {Array.from({ length: count }, (_, i) => (
          <div key={i} className="app-org-skeleton">
            <Skeleton width="45%" />
            <Skeleton width="25%" height={12} />
          </div>
        ))}
      </div>
    </div>
  );
}

export function None({ children }: { children: ReactNode }) {
  return (
    <div className="app-org-list">
      <div className="app-org-none">
        <Text variant="caption" tone="muted">
          {children}
        </Text>
      </div>
    </div>
  );
}

/** Hands a fetched file to the browser as a download. The anchor never enters the page's markup. */
export function saveFile(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

export function formatBytes(n: number): string {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return Math.round(n / 1024) + ' KB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}
