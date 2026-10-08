const { NavItem, OrgSwitcher, AgentAvatar, AgentState, Button, Icon } = window.KivaliDesignSystem_9b9983;

const ORGS = [
  { id: 'sunset', name: 'Sunset Gardens', color: 'pine' },
  { id: 'personal', name: 'Personal', color: 'lake' },
];

function Sidebar({ view, setView, agents, onHire }) {
  const [org, setOrg] = React.useState('sunset');
  return (
    <aside style={{ width: 'var(--sidebar-width)', flex: 'none', height: '100%', boxSizing: 'border-box', padding: 'var(--space-4) 12px', borderRight: '1px solid var(--line)', display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', background: 'var(--paper)' }}>
      <OrgSwitcher orgs={ORGS} current={org} onSelect={setOrg} />
      <nav style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <NavItem icon="inbox" label="Inbox" count={2} attention active={view === 'inbox'} onClick={() => setView('inbox')} />
        <NavItem icon="list-todo" label="Work" count={7} active={view === 'work'} onClick={() => setView('work')} />
        <NavItem icon="users" label="Team" active={view === 'team'} onClick={() => setView('team')} />
        <NavItem icon="network" label="Knowledge" active={view === 'graph'} onClick={() => setView('graph')} />
      </nav>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <div style={{ font: 'var(--type-label)', letterSpacing: '.02em', color: 'var(--ink-muted)', padding: '0 10px 6px' }}>Agents · {agents.length}</div>
        {agents.map((a) => (
          <NavItem key={a.id} label={a.name} active={view === 'chat:' + a.id} onClick={() => setView('chat:' + a.id)}
            lead={<AgentAvatar name={a.name} role={a.role} color={a.color} size={20} />}
            count={a.state === 'running' ? <AgentState state="running" compact /> : null} />
        ))}
      </div>
      <div style={{ marginTop: 'auto', display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Button variant="secondary" icon={<Icon name="user-plus" />} onClick={onHire}>Hire agent</Button>
        <NavItem icon="settings" label="Settings" />
      </div>
    </aside>
  );
}
window.Sidebar = Sidebar;
