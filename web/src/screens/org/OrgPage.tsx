import { useEffect, useRef, useState } from 'react';
import type { ComponentType } from 'react';
import { Link, useHref, useNavigate, useParams } from 'react-router';
import type { Usage } from '../../api/types.gen';
import { useFrameChrome } from '../../app/chrome';
import { ORG_LOGO_SRC, orgName } from '../../app/orgIdentity';
import { PageTitle } from '../../app/PageTitle';
import { BREAKPOINT_TABBAR, Icon, ListRow, NavItem, OrgMark, Skeleton, Text, mediaQuery } from '../../ds';
import { useMediaQuery } from '../../lib/useMediaQuery';
import { useScrollAnchor } from '../../lib/useScrollAnchor';
import { useScrollSpy } from '../../lib/useScrollSpy';
import { flattenTree } from '../../state/org';
import { useOrg } from '../../state/OrgProvider';
import { AboutSection, versionLabel } from './About';
import { HandbookSection } from './Handbook';
import { FilesSection } from './Files';
import { NetworkSection } from './Lists';
import { OrganizationSection } from './Organization';
import { SkillsSection } from './Skills';
import { BackupSection } from './Backup';
import { SECTIONS, sectionByKey } from './sections';
import type { SectionInfo, SectionKey, SectionProps } from './sections';
import { USAGE_PATH, UsageSection, money } from './Usage';
import { useResource } from './useResource';
import '../../styles/org.css';

const BODIES: Record<SectionKey, ComponentType<SectionProps>> = {
  usage: UsageSection,
  organization: OrganizationSection,
  handbook: HandbookSection,
  files: FilesSection,
  skills: SkillsSection,
  network: NetworkSection,
  backup: BackupSection,
  about: AboutSection,
};

/** The longest a clicked index item is held while the smooth scroll travels. */
const PICK_HOLD_MS = 1500;
const SCROLL_KEYS = new Set(['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' ', 'Spacebar']);

function sectionPath(key: SectionKey): string {
  return '/org/' + key;
}

/** Element ids of the desktop sections, in page order, and the marker after the last one. */
const SECTION_IDS = SECTIONS.map((s) => sectionId(s.key));
const END_ID = 'org-end';

function sectionId(key: SectionKey): string {
  return 'org-' + key;
}

function keyOfId(id: string): SectionKey | undefined {
  return SECTIONS.find((s) => sectionId(s.key) === id)?.key;
}

/** A plain left click: anything else (a new tab, a download) stays the browser's. */
interface IndexClick {
  button: number;
  metaKey: boolean;
  ctrlKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
  defaultPrevented: boolean;
  preventDefault(): void;
}

function IndexLink({ info, active, onPick }: { info: SectionInfo; active: boolean; onPick(key: SectionKey): void }) {
  const href = useHref(sectionPath(info.key));
  // NavItem hands its anchor's click event through; the declared `(): void` just does not name it.
  const onClick = (ev?: IndexClick) => {
    if (!ev || ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    ev.preventDefault();
    onPick(info.key);
  };
  return <NavItem icon={info.icon} label={info.label} href={href} active={active} onClick={onClick} />;
}

function prefersReducedMotion(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

/**
 * The desktop page. The index highlights the section being read (a scroll spy); `/org/:section` scrolls to its
 * section when the route changes from outside the index. A click on the index navigates and scrolls; until that
 * scroll arrives (the spy reports the clicked section, or the scroll ends) the clicked item stays highlighted, so
 * the sections passed on the way do not flicker through it. Either scroll keeps its section at the top while the
 * sections load and the page grows, until the reader scrolls. Scrolling by hand never touches the URL.
 */
function DesktopOrg({ routeKey }: { routeKey: SectionKey | undefined }) {
  const navigate = useNavigate();
  const spyId = useScrollSpy({ ids: SECTION_IDS, fallback: sectionId(routeKey ?? 'usage'), endId: END_ID });
  const spied = keyOfId(spyId) ?? 'usage';
  const [picked, setPicked] = useState<SectionKey | null>(null);
  // The route change an index click is about to cause; its scroll is already under way.
  const clicked = useRef<SectionKey | null>(null);
  // Each section loads its own list, so the page grows after the first scroll; keep the scrolled-to section in place.
  const sections = useRef<HTMLDivElement>(null);
  const anchor = useScrollAnchor(sections);

  // The travel has arrived: hand the highlight back to the spy.
  if (picked !== null && spied === picked) setPicked(null);

  useEffect(() => {
    if (picked === null) return;
    const done = () => setPicked(null);
    // `scrollend` is missing from Safari and WKWebView before 26.2, and never fires when the click needs no scroll
    // (the section is already at the line) or the page cannot reach it; the spy may also never report the clicked
    // section (the bottom of the page belongs to the last one). So the hold also ends on a timeout and at the
    // reader's first wheel, touch, scrolling key or press on the scrollbar.
    const timer = window.setTimeout(done, PICK_HOLD_MS);
    const onKey = (ev: KeyboardEvent) => {
      if (SCROLL_KEYS.has(ev.key)) done();
    };
    const opts = { capture: true, passive: true } as const;
    document.addEventListener('scrollend', done);
    window.addEventListener('wheel', done, opts);
    window.addEventListener('touchstart', done, opts);
    window.addEventListener('keydown', onKey, opts);
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener('scrollend', done);
      window.removeEventListener('wheel', done, opts);
      window.removeEventListener('touchstart', done, opts);
      window.removeEventListener('keydown', onKey, opts);
    };
  }, [picked]);

  useEffect(() => {
    if (!routeKey) return;
    if (clicked.current === routeKey) {
      clicked.current = null;
      return;
    }
    const el = document.getElementById(sectionId(routeKey));
    if (el) anchor(el, 'auto');
  }, [routeKey, anchor]);

  const pick = (key: SectionKey) => {
    setPicked(key);
    if (key !== routeKey) {
      clicked.current = key;
      void navigate(sectionPath(key));
    }
    const el = document.getElementById(sectionId(key));
    if (el) anchor(el, prefersReducedMotion() ? 'auto' : 'smooth');
  };

  const active = picked ?? spied;
  return (
    <div className="app-org">
      <nav className="app-org-index" aria-label="Org sections">
        {SECTIONS.map((s) => (
          <IndexLink key={s.key} info={s} active={s.key === active} onPick={pick} />
        ))}
      </nav>
      <div ref={sections} className="app-org-sections">
        <OrgHeader />
        {SECTIONS.map((s) => {
          const Body = BODIES[s.key];
          return (
            <section key={s.key} id={sectionId(s.key)} className="app-org-section" aria-label={s.label}>
              <Body phone={false} showHead />
            </section>
          );
        })}
        <div id={END_ID} className="app-org-end" aria-hidden="true" />
      </div>
    </div>
  );
}

function PageRow({ info, meta }: { info: SectionInfo; meta?: string | undefined }) {
  const href = useHref(sectionPath(info.key));
  return <ListRow lead={<Icon name={info.icon} size={20} />} title={info.label} meta={meta ?? info.blurb} trail={<Icon name="chevron-right" />} href={href} />;
}

/** "Org settings · you and 6 agents" */
export function headerMeta(agents: number): string {
  return `Org settings · you and ${agents} ${agents === 1 ? 'agent' : 'agents'}`;
}

/** Canvas 6a's header: the org's mark and name, and who is in it. The page's one h1. */
function OrgHeader() {
  const { me, org } = useOrg();
  const name = orgName(me);
  return (
    <div className="app-org-header">
      <OrgMark name={name} size={40} {...(me?.org.has_logo ? { src: ORG_LOGO_SRC } : {})} />
      <div>
        <PageTitle>{name}</PageTitle>
        <Text variant="label" tone="muted">
          {headerMeta(flattenTree(org.tree).length)}
        </Text>
      </div>
    </div>
  );
}

/** The phone's Org home: today's spend at a glance (it opens Usage), then one row per sub-page. */
function PhoneHome() {
  const { me } = useOrg();
  const usage = useResource<Usage>(USAGE_PATH);
  const tiles = usage.data?.tiles;
  const cells = [
    { label: 'Today', value: tiles?.today.spend },
    { label: '7 days', value: tiles?.d7.spend },
    { label: '30 days', value: tiles?.d30.spend },
  ];
  return (
    <div className="app-org-phone">
      <Link className="app-org-glance" to={sectionPath('usage')}>
        <div className="app-org-glance-cells">
          {cells.map((c) => (
            <div key={c.label} className="app-org-glance-cell">
              <Text variant="label" tone="muted">
                {c.label}
              </Text>
              {c.value === undefined ? <Skeleton width="60%" height={18} /> : <Text variant="body">{money(c.value)}</Text>}
            </div>
          ))}
        </div>
        <Text variant="caption" tone="muted" className="app-org-glance-go">
          Usage
          <Icon name="chevron-right" />
        </Text>
      </Link>
      <div className="app-org-list">
        <div className="app-org-rows">
          {SECTIONS.filter((s) => s.key !== 'usage').map((s) => (
            <PageRow key={s.key} info={s} meta={s.key === 'about' && me?.version ? versionLabel(me.version) : undefined} />
          ))}
        </div>
      </div>
    </div>
  );
}

/**
 * Org. Desktop (960 and up) is one sectioned page with a left index; `/org/:section` scrolls to that section.
 * Phone is a list of sub-pages: `/org` lists them and `/org/:section` shows one with a back link to Org.
 */
export function Org() {
  const { section } = useParams();
  const desktop = useMediaQuery(mediaQuery(BREAKPOINT_TABBAR));
  const info = sectionByKey(section);
  const key = info?.key;

  useFrameChrome(!desktop && info ? { title: info.label, back: { to: '/org', label: 'Org' } } : { title: 'Org' });

  if (!desktop) {
    if (!info) return <PhoneHome />;
    const Body = BODIES[info.key];
    return (
      <div className="app-org-sub">
        <Body phone showHead={false} />
      </div>
    );
  }

  return <DesktopOrg routeKey={key} />;
}
