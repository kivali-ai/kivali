const K_work = window.KivaliDesignSystem_9b9983;

function PageHead({ title, meta, actions }) {
  return (
    <div style={{ display: 'flex', alignItems: 'flex-end', gap: 'var(--space-4)', marginBottom: 'var(--space-6)' }}>
      <div style={{ flex: 1 }}>
        <h1 style={{ margin: 0, font: 'var(--type-title)', letterSpacing: '-0.02em' }}>{title}</h1>
        {meta && <div style={{ marginTop: 4, font: 'var(--type-caption)', color: 'var(--ink-muted)' }}>{meta}</div>}
      </div>
      {actions}
    </div>
  );
}
const page = (w) => ({ maxWidth: w, margin: '0 auto', padding: 'var(--space-8)' });

function InboxScreen({ openChat, onToast }) {
  const { Card, Button, Badge, AgentAvatar, AgentState, ListRow, Banner, Tabs } = K_work;
  const [approved, setApproved] = React.useState(false);
  return (
    <div style={page('var(--content)')}>
      <PageHead title="Inbox" meta="2 need a decision · 5 updates" />
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <Banner tone="warning" title="Couldn't reach the weather service.">Retrying in 5 minutes.</Banner>
        {!approved ? (
          <Card tone="attention" title="Approve a $48 seed order?" meta={<><AgentAvatar name="Supplier scout" role="search" color="clay" size={20} /><span>Supplier scout · 12 minutes ago</span><Badge tone="signal">Needs you</Badge></>}
            actions={<><Button size="sm">Ask why</Button><Button size="sm" variant="primary" onClick={() => { setApproved(true); onToast('Order approved', 'Supplier scout will place it this afternoon.'); }}>Approve</Button></>}>
            Tomato, basil and bean seeds for the raised beds, from the local seed co-op. It compared three suppliers; this one has the best germination rates.
          </Card>
        ) : null}
        <Card title="Weekly review" meta="Chief of Staff · Monday" collapsible defaultOpen={false}>
          Your gardener learned 3 things this week. Bookkeeper closed March with no open questions.
        </Card>
        <div style={{ background: 'var(--paper-raised)', border: '1px solid var(--line)', borderRadius: 'var(--radius-lg)', overflow: 'hidden' }}>
          <ListRow onClick={() => openChat('garden')} lead={<AgentAvatar name="Garden advisor" role="sprout" color="olive" size={32} />} title="Hold off watering bed 3 today" meta={<><AgentState state="running" /><span>Garden advisor</span></>} trail="09:06" />
          <ListRow onClick={() => openChat('books')} lead={<AgentAvatar name="Bookkeeper" role="calculator" color="ochre" size={32} />} title="March closed with no open questions" meta={<><AgentState state="done" /><span>Bookkeeper</span></>} trail="Yesterday" />
          <ListRow onClick={() => openChat('cos')} lead={<AgentAvatar name="Chief of Staff" role="compass" color="iris" size={32} />} title="Drafted next week's priorities" meta={<><AgentState state="idle" /><span>Chief of Staff</span></>} trail="Mon" />
        </div>
      </div>
    </div>
  );
}

function WorkScreen() {
  const { AssignmentRow, Tabs, Button, Icon, AcceptanceMeter } = K_work;
  const [open, setOpen] = React.useState(true);
  const [sel, setSel] = React.useState(14);
  const scout = { name: 'Supplier scout', role: 'search', color: 'clay' };
  const garden = { name: 'Garden advisor', role: 'sprout', color: 'olive' };
  const rows = (
    <div style={{ background: 'var(--paper-raised)', border: '1px solid var(--line)', borderRadius: 'var(--radius-lg)', overflow: 'hidden' }}>
      <AssignmentRow id={10} title="Get the raised beds ready for spring" state="ready" assignee={garden} openChildren={3} acceptance={{ satisfied: 1, claimed: 1, unclaimed: 1 }} expanded={open} onToggle={() => setOpen(!open)} onClick={() => setSel(10)} selected={sel === 10} />
      {open && <>
        <AssignmentRow id={12} depth={1} title="Order the tomato seeds" state="ready" assignee={scout} onClick={() => setSel(12)} selected={sel === 12} />
        <AssignmentRow id={14} depth={1} title="Plant out the tomato seedlings" state="blocked" assignee={garden} waitingOn={[{ id: 12, state: 'ready' }]} onClick={() => setSel(14)} selected={sel === 14} />
        <AssignmentRow id={15} depth={1} last title="Pick a drip-line layout" state="held" heldBy="Maya" assignee={{ kind: 'person', name: 'Maya Chen' }} onClick={() => setSel(15)} selected={sel === 15} />
      </>}
      <AssignmentRow id={8} title="Close March books" state="done" assignee={{ name: 'Bookkeeper', role: 'calculator', color: 'ochre' }} onClick={() => setSel(8)} selected={sel === 8} />
      <AssignmentRow id={6} title="Try chain-store seeds in bed 3" state="dropped" assignee={garden} onClick={() => setSel(6)} selected={sel === 6} />
    </div>
  );
  return (
    <div style={page('var(--content-wide)')}>
      <PageHead title="Work" meta="7 open · 1 waiting on you" actions={<Button variant="primary" icon={<Icon name="plus" />}>New assignment</Button>} />
      <Tabs tabs={[{ value: 'open', label: 'Open', count: 7, content: rows }, { value: 'done', label: 'Done', count: 23, content: null }]} />
    </div>
  );
}

function TeamScreen({ agents, openChat, onHire }) {
  const { Table, AgentAvatar, AgentState, Badge, Button, Icon } = K_work;
  return (
    <div style={page('var(--content-wide)')}>
      <PageHead title="Team" meta={agents.length + ' agents · 1 person'} actions={<Button variant="primary" icon={<Icon name="user-plus" />} onClick={onHire}>Hire agent</Button>} />
      <Table rows={agents} columns={[
        { key: 'name', label: 'Agent', render: (a) => <span onClick={() => openChat(a.id)} style={{ display: 'flex', alignItems: 'center', gap: 10, cursor: 'pointer', fontWeight: 500 }}><AgentAvatar name={a.name} role={a.role} color={a.color} size={24} />{a.name}</span> },
        { key: 'reports', label: 'Reports to' },
        { key: 'state', label: 'State', render: (a) => <AgentState state={a.state} /> },
        { key: 'notes', label: 'Memory', align: 'right', mono: true },
        { key: 'learned', label: 'This week', render: (a) => a.learned ? <Badge tone="cobalt">Learned {a.learned}</Badge> : <span style={{ color: 'var(--ink-muted)' }}>—</span> },
      ]} />
    </div>
  );
}

function GraphScreen() {
  const { NodeChip, EmptyState } = K_work;
  return (
    <div style={page('var(--content)')}>
      <PageHead title="Knowledge" meta="Decisions, requirements and artifacts your team rests on" />
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10 }}>
        <NodeChip id="decision/seed-supplier" kind="decision" summary="Local seed co-op, one order" owner="iris" ownerName="Chief of Staff" selected />
        <NodeChip id="req/seed-budget" kind="requirement" summary="Under $50 this season" owner="ochre" ownerName="Bookkeeper" />
        <NodeChip id="req/tool-policy" kind="requirement" summary="Weatherproof, 5 year life" />
        <NodeChip id="decision/chain-store-seeds" kind="decision" status="superseded" summary="Replaced by the co-op" />
        <NodeChip id="coop-catalogue.pdf" kind="artifact" summary="Seed co-op, 9 pages" owner="clay" />
        <NodeChip id="decision/drip-line" kind="decision" status="unresolved" summary="Missing req/drip-budget" />
      </div>
    </div>
  );
}
Object.assign(window, { InboxScreen, WorkScreen, TeamScreen, GraphScreen });
