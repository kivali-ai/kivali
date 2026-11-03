import { createContext, useContext, useMemo } from 'react';
import type { AttachmentRef, PersonRef } from '../../api/types.gen';
import { attachmentAction, useOpenAttachment } from '../../app/attachments';
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

/** Attachments as secondary sm buttons: "hosting-quote.pdf · 84 KB". Markdown opens as a document; the rest download. */
export function Attachments({ items }: { items: readonly AttachmentRef[] }) {
  const open = useOpenAttachment();
  if (items.length === 0) return null;
  return (
    <div className="app-home-files">
      {items.map((a) => {
        const size = formatSize(a.size_bytes);
        const { icon, title } = attachmentAction(a);
        return (
          <Button key={a.url + a.name} variant="secondary" size="sm" icon={<Icon name={icon} />} title={title} onClick={() => open(a)}>
            {size ? a.name + ' · ' + size : a.name}
          </Button>
        );
      })}
    </div>
  );
}
