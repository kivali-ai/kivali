import type { ReactNode } from 'react';
import { AgentState, AutoRelease, OrgMark, PersonAvatar, Text } from '../ds';
import { autoReleaseLabel, autoReleaseValue } from '../state/org';
import { useOrg } from '../state/OrgProvider';
import { AccountMenu } from './AccountMenu';
import { DESTINATIONS } from './nav';
import type { DestinationKey } from './nav';
import { NameSep, NavLinkItem } from './NavLinkItem';
import { ORG_LOGO_SRC, orgName } from './orgIdentity';
import { TeamTree } from './TeamTree';

/**
 * Home's label as the canvas draws it: the word, then a compact running AgentState beside it while anything
 * runs. The Needs-you count goes in NavItem's count slot with `attention` (the frame's one signal pill).
 */
export function homeLabel(label: string, running: boolean): ReactNode {
  if (!running) return label;
  return (
    <>
      {label}
      <NameSep />
      <span className="app-nav-running">
        <AgentState state="running" compact />
      </span>
    </>
  );
}

/** Home's count slot: the Needs-you number, with a spoken separator so the name is "Home, 3". */
function homeCount(needs: number): ReactNode {
  if (needs <= 0) return null;
  return (
    <>
      <NameSep />
      {needs}
    </>
  );
}

export interface SidebarProps {
  active: DestinationKey | null;
  activeAgent: string | null;
  onSignOut(): void;
}

/** Desktop only (the stylesheet hides it below 960): the org's mark and name, five destinations, team tree, foot. */
export function Sidebar({ active, activeAgent, onSignOut }: SidebarProps) {
  const { me, org, setAutoRelease } = useOrg();
  const person = me?.user.name ?? '';

  return (
    <aside className="app-sidebar" aria-label="Sidebar">
      <div className="app-sidebar-org">
        <OrgMark name={orgName(me)} size={28} {...(me?.org.has_logo ? { src: ORG_LOGO_SRC } : {})} />
        <span className="app-sidebar-org-name">{orgName(me)}</span>
      </div>
      <nav className="app-sidebar-nav" aria-label="Main">
        {DESTINATIONS.map((d) => {
          const home = d.key === 'home';
          const needs = home ? org.needs : 0;
          return (
            <NavLinkItem
              key={d.key}
              to={d.to}
              icon={d.icon}
              label={home ? homeLabel(d.label, org.running) : d.label}
              active={d.key === active}
              count={homeCount(needs)}
              attention={needs > 0}
            />
          );
        })}
      </nav>
      <hr className="app-hairline" />
      <div className="app-sidebar-tree">
        <TeamTree tree={org.tree} person={person} activeSlug={activeAgent} />
      </div>
      <div className="app-sidebar-foot">
        <AutoRelease
          value={autoReleaseLabel(org.autoRelease)}
          onChange={(label) => {
            const value = autoReleaseValue(label);
            if (value) void setAutoRelease(value);
          }}
        />
        <div className="app-account">
          <PersonAvatar name={person || 'You'} size={28} />
          <Text className="app-account-name">{person}</Text>
          <AccountMenu onSignOut={onSignOut} />
        </div>
      </div>
    </aside>
  );
}
