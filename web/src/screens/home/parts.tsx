import { createContext, useContext, useMemo } from 'react';
import type { AttachmentRef, PersonRef } from '../../api/types.gen';
import { AgentAvatar, Button, Icon, PersonAvatar, Prose, Skeleton } from '../../ds';
import { agentIdentity } from '../../lib/agentIdentity';
import type { Identify } from '../../lib/agentIdentity';
import { renderMarkdown } from '../../lib/markdown';
import { formatSize } from '../../state/home';
import { usePersonName } from '../../state/OrgProvider';

/** The slug the server gives you in PersonRef. */
export const YOU = 'ceo';

const IdentityContext = createContext<Identify>((who) => agentIdentity({ slug: who.slug, name: who.name ?? '' }));

export const IdentityProvider = IdentityContext.Provider;

export function useIdentify(): Identify {
  return useContext(IdentityContext);
}

/** An agent's tile, or your circle: people are never drawn as agents. */
export function Face({ who, size }: { who: PersonRef; size: 20 | 24 | 32 }) {
  const identify = useIdentify();
  const personName = usePersonName();
  return who.slug === YOU ? <PersonAvatar name={personName} size={size} /> : <AgentAvatar {...identify(who)} size={size} />;
}

/** A message body: markdown, sanitized, set as Prose. Nothing for an empty body. */
export function Body({ markdown }: { markdown: string }) {
  const html = useMemo(() => renderMarkdown(markdown), [markdown]);
  if (!html) return null;
  return <Prose html={html} />;
}

function download(url: string): void {
  window.open(url, '_blank', 'noopener');
}

/** Placeholder rows while a list loads: an avatar tile and two lines, shaped like the rows that replace them. */
export function RowSkeletons({ count = 3 }: { count?: number }) {
  return (
    <>
      {Array.from({ length: count }, (_, i) => (
        <div key={i} className="app-home-skeleton">
          <Skeleton width={32} height={32} />
          <div className="app-home-skeleton-text">
            <Skeleton width="60%" />
            <Skeleton width="35%" height={12} />
          </div>
        </div>
      ))}
    </>
  );
}

/** Attachments as secondary sm buttons with a download icon: "hosting-quote.pdf · 84 KB". */
export function Attachments({ items }: { items: readonly AttachmentRef[] }) {
  if (items.length === 0) return null;
  return (
    <div className="app-home-files">
      {items.map((a) => {
        const size = formatSize(a.size_bytes);
        return (
          <Button key={a.url + a.name} variant="secondary" size="sm" icon={<Icon name="download" />} title={'Download ' + a.name} onClick={() => download(a.url)}>
            {size ? a.name + ' · ' + size : a.name}
          </Button>
        );
      })}
    </div>
  );
}
