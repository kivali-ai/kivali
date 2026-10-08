import { useState } from 'react';
import type { ReactNode } from 'react';
import {
  AcceptanceMeter,
  AgentAvatar,
  AgentState,
  AutoRelease,
  Badge,
  Banner,
  Button,
  Card,
  ChatPager,
  Checkbox,
  CodeBlock,
  Composer,
  ContextCount,
  ContextGauge,
  DayBars,
  Dialog,
  DialogClose,
  DocDiff,
  EmptyState,
  FileDrop,
  GoalRow,
  Icon,
  AssignmentRef,
  AssignmentRow,
  AssignmentState,
  ListRow,
  Menu,
  Message,
  NavItem,
  NodeChip,
  NodeRow,
  OrgMark,
  PersonAvatar,
  Progress,
  Prose,
  QueueRow,
  RankedBars,
  Readouts,
  Select,
  Skeleton,
  SubagentGroup,
  SubagentTask,
  Switch,
  Table,
  Tabs,
  Text,
  TextField,
  Thinking,
  Toast,
  ToolCall,
  Tooltip,
  TooltipProvider,
  colorFor,
  iconNames,
  identityColors,
  roleIconNames,
  AUTO_RELEASE_STOPS,
} from '../../ds';
import type { AgentRunState, AssignmentReadiness } from '../../ds';
import { applyTheme, getStoredTheme } from '../../app/theme';
import type { ThemeChoice } from '../../app/theme';
import '../../styles/gallery.css';

const CS = { name: 'Chief of Staff', role: 'compass', color: 'iris' };
const SS = { name: 'Supplier scout', role: 'search', color: 'clay' };
const BK = { name: 'Bookkeeper', role: 'calculator', color: 'ochre' };
const GA = { name: 'Garden advisor', role: 'sprout', color: 'olive' };

const RUN_STATES: AgentRunState[] = ['running', 'idle', 'queued', 'blocked', 'held', 'errored', 'done', 'cancelled'];
const ASSIGNMENT_STATES: AssignmentReadiness[] = ['ready', 'blocked', 'held', 'done', 'dropped'];

const LOGO =
  'data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA2NCA2NCI+PHJlY3Qgd2lkdGg9IjY0IiBoZWlnaHQ9IjY0IiByeD0iMTQiIGZpbGw9IiMyRTVFM0EiLz48cGF0aCBkPSJNMzIgNTBjMC0xNCA2LTI0IDE4LTMwLTIgMTQtOCAyNC0xOCAzMHoiIGZpbGw9IiNFOEQzOEEiLz48cGF0aCBkPSJNMzIgNTBjMC0xMi01LTIwLTE1LTI1IDEgMTIgNiAyMCAxNSAyNXoiIGZpbGw9IiNBOEM5OEEiLz48Y2lyY2xlIGN4PSIzMiIgY3k9IjUwIiByPSIzIiBmaWxsPSIjRThEMzhBIi8+PC9zdmc+';

const SPEND = [12, 18, 9, 22, 31, 14, 8, 19, 25, 27, 16, 11, 34, 29, 21, 17, 13, 24, 38, 26, 20, 15, 10, 23, 30, 28, 19, 12, 9, 17];

const DIFF_BEFORE = 'Chief of Staff keeps the garden running.\n## Priorities\n- Water the east beds daily\n- Order seeds\n## Budget\n- Tools under $100';
const DIFF_AFTER =
  'Chief of Staff keeps the garden running.\n## Priorities\n- Water the east beds when it has not rained for two days\n- Order seeds\n- Review the planting calendar weekly\n## Budget\n- Tools under $100';

const NODES = [
  { node: { id: 'decision/seed-supplier', kind: 'decision' as const, owner: 'iris', ownerName: 'Chief of Staff' }, short: 'Local seed co-op', fields: [['summary', 'Buy from the local seed co-op, not the big chain'], ['owner', 'Chief of Staff'], ['status', 'active']] as [string, ReactNode][] },
  { node: { id: 'req/seed-budget', kind: 'requirement' as const, owner: 'ochre', ownerName: 'Bookkeeper' }, short: 'Under $50', fields: [['summary', 'Seeds under $50 a season']] as [string, ReactNode][] },
  { node: { id: 'decision/chain-store-seeds', kind: 'decision' as const, status: 'superseded' as const }, short: 'Superseded', fields: [] as [string, ReactNode][] },
  { node: { id: 'garden-plan-2026.pdf', kind: 'artifact' as const }, short: 'v3', fields: [] as [string, ReactNode][] },
];

interface ComponentProps {
  name: string;
  children: ReactNode;
}

function Component({ name, children }: ComponentProps) {
  return (
    <section className="app-component" id={name}>
      <Text as="h3" variant="heading" className="app-component-title">
        {name}
      </Text>
      {children}
    </section>
  );
}

function Spec({ label, children }: { label?: string; children: ReactNode }) {
  return (
    <div className="app-spec">
      {label && (
        <Text as="p" variant="label" tone="muted" className="app-spec-label">
          {label}
        </Text>
      )}
      {children}
    </div>
  );
}

function Group({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="app-group">
      <Text as="h2" variant="title" className="app-group-title">
        {title}
      </Text>
      {children}
    </section>
  );
}

function AutoReleaseDemo({ initial, variant }: { initial: string; variant?: 'slider' | 'select' }) {
  const [v, setV] = useState(initial);
  return <AutoRelease value={v} onChange={setV} variant={variant} />;
}

function CheckboxDemo() {
  const [a, setA] = useState<boolean | 'indeterminate'>(true);
  return (
    <div className="app-col">
      <Checkbox label="Auto-release messages from this agent" checked={a} onCheckedChange={setA} />
      <Checkbox label="Include past chats" hint="Adds archived conversations to the agent's files." />
      <Checkbox label="Select all" checked="indeterminate" />
      <Checkbox label="Not available" disabled />
    </div>
  );
}

function SwitchDemo() {
  const [a, setA] = useState(true);
  return (
    <div className="app-col">
      <Switch label="Research skill" hint="Available to every agent." checked={a} onCheckedChange={setA} />
      <Switch label="Auto-release" />
      <Switch label="Not available" disabled />
    </div>
  );
}

function ChatPagerDemo({ compact }: { compact: boolean }) {
  const [i, setI] = useState(3);
  return (
    <ChatPager
      title="Watering schedule for the east beds"
      index={i}
      total={7}
      compact={compact}
      onOlder={() => setI(i - 1)}
      onNewer={() => setI(i + 1)}
      onCurrent={i < 7 ? () => setI(7) : undefined}
    />
  );
}

function QueueDemo() {
  const [sel, setSel] = useState(false);
  const [open, setOpen] = useState(true);
  return (
    <div className="app-box app-narrow">
      <QueueRow from={CS} to={[SS]} kind="assignment" refId={12} title="Price out three seed suppliers" releasesIn={24} expanded={open} onToggle={() => setOpen(!open)} onRelease={() => {}}>
        <Prose>
          <p>Get quotes from three seed suppliers for tomatoes, basil and beans. Keep the total under $50.</p>
        </Prose>
      </QueueRow>
      <QueueRow from={BK} to={[CS, GA]} title="Budget updated: $120 left for tools this month" releasesIn={0} selectable selected={sel} onSelect={(v) => setSel(!!v)} onRelease={() => {}} />
      <QueueRow from={GA} to={[CS]} title="Rain forecast changed; watering paused" held onRelease={() => {}} />
    </div>
  );
}

function NodeRowDemo({ compact }: { compact: boolean }) {
  const [open, setOpen] = useState(0);
  return (
    <div className="app-box app-narrow">
      {NODES.map((n, i) => (
        <NodeRow key={n.node.id} {...n} compact={compact} expanded={open === i} onToggle={() => setOpen(open === i ? -1 : i)} />
      ))}
    </div>
  );
}

function DialogDemo() {
  return (
    <div className="app-row">
      <Dialog
        trigger={<Button variant="danger">Offboard</Button>}
        tone="danger"
        title="Offboard Garden advisor?"
        description="Its files move to the archive. Nothing is deleted, and you can restore it later."
        footer={
          <>
            <DialogClose asChild>
              <Button>Cancel</Button>
            </DialogClose>
            <Button variant="danger">Offboard</Button>
          </>
        }
      />
      <Dialog
        trigger={<Button>Rename agent</Button>}
        title="Rename agent"
        footer={
          <>
            <DialogClose asChild>
              <Button>Cancel</Button>
            </DialogClose>
            <Button variant="primary">Save</Button>
          </>
        }
      >
        <TextField label="Agent name" placeholder="Garden advisor" />
      </Dialog>
    </div>
  );
}

const TABLE_ROWS = [
  { id: 1, name: 'Seed catalogue.pdf', kind: <Badge>Project file</Badge>, size: '2.4 MB', when: 'Sep 21' },
  { id: 2, name: 'Planting calendar 2026.md', kind: <Badge tone="cobalt">Artifact</Badge>, size: '18 KB', when: 'Sep 19' },
  { id: 3, name: 'Handbook.md', kind: <Badge>System</Badge>, size: '6 KB', when: 'Sep 02' },
];

function Core({ phone }: { phone: boolean }) {
  return (
    <Group title="Core">
      <Component name="Text">
        <Spec label="Variants">
          <div className="app-col-tight">
            <Text as="div" variant="display">$1,240</Text>
            <Text as="div" variant="title">Inbox</Text>
            <Text as="div" variant="heading">Waiting on you</Text>
            <Text as="p" variant="body">Get quotes from three seed suppliers for tomatoes, basil and beans.</Text>
            <Text as="p" variant="caption" tone="muted">Updated 4 minutes ago</Text>
            <Text as="div" variant="label" tone="muted">Opus · high</Text>
            <Text as="div" variant="code">decision/seed-supplier</Text>
          </div>
        </Spec>
        <Spec label="Tones">
          <div className="app-row">
            <Text tone="default">Default</Text>
            <Text tone="muted">Muted</Text>
            <Text tone="faint">Faint</Text>
            <Text tone="signal">Signal</Text>
            <Text tone="cobalt">Cobalt</Text>
            <Text tone="success">Success</Text>
            <Text tone="danger">Danger</Text>
          </div>
        </Spec>
      </Component>
      <Component name="Icon">
        <Spec>
          <div className="app-row">
            <Icon name="inbox" size={16} />
            <Icon name="inbox" size={20} />
            <Icon name="inbox" size={24} />
            <Text variant="label" tone="muted">16, 20 and 24px</Text>
          </div>
        </Spec>
        <Spec label={iconNames.length + ' icons'}>
          <div className="app-icon-grid">
            {iconNames.map((n) => (
              <div className="app-icon-cell" key={n}>
                <Icon name={n} size={20} />
                <span className="app-icon-name">{n}</span>
              </div>
            ))}
          </div>
        </Spec>
      </Component>

      <Component name="Button">
        {(
          [
            ['Medium, 36 pixels, the default', 'md'],
            ['Small, 28 pixels, dense rows and cards', 'sm'],
          ] as const
        ).map(([label, size]) => (
          <Spec key={size} label={label}>
            <div className="app-row">
              <Button variant="primary" size={size}>Hire agent</Button>
              <Button size={size}>Brief</Button>
              <Button variant="ghost" size={size}>Skip</Button>
              <Button variant="danger" size={size}>Offboard</Button>
            </div>
          </Spec>
        ))}
        <Spec label="States">
          <div className="app-row">
            <Button variant="primary" loading>Saving</Button>
            <Button disabled>Disabled</Button>
            <Button iconOnly icon={<Icon name="ellipsis" />} aria-label="More" />
            <Button variant="primary" icon={<Icon name="user-plus" />}>With icon</Button>
          </div>
        </Spec>
      </Component>

      <Component name="TextField">
        <div className="app-col app-narrow">
          <TextField label="Role name" placeholder="Garden advisor" hint="Shown on the org chart." />
          <TextField label="Brief" multiline rows={3} defaultValue="Check the forecast each morning." error="Keep briefs under 500 characters." />
        </div>
      </Component>

      <Component name="Select">
        <div className="app-narrow">
          <Select
            label="Model"
            defaultValue="sonnet"
            hint="Applies from the next turn."
            options={[
              { value: 'opus', label: 'Opus' },
              { value: 'sonnet', label: 'Sonnet' },
              { value: 'haiku', label: 'Haiku' },
            ]}
          />
        </div>
      </Component>

      <Component name="Checkbox">
        <CheckboxDemo />
      </Component>

      <Component name="Switch">
        <SwitchDemo />
      </Component>

      <Component name="Badge">
        <Spec label="Tones">
          <div className="app-row">
            <Badge>Draft</Badge>
            <Badge tone="signal">Needs you</Badge>
            <Badge tone="cobalt">Learned 3 things</Badge>
            <Badge tone="success">Approved</Badge>
            <Badge tone="danger">Failed</Badge>
          </div>
        </Spec>
        <Spec label="Variants">
          <div className="app-row">
            <Badge tone="signal" variant="solid">4</Badge>
            <Badge variant="solid">You</Badge>
            <Badge variant="outline" mono>opus · high</Badge>
            <Badge variant="outline" mono>#142</Badge>
          </div>
        </Spec>
      </Component>

      <Component name="Card">
        <div className="app-col app-narrow">
          <Card
            title="Hire a researcher"
            tone="attention"
            meta={
              <>
                <Badge tone="signal">Needs your approval</Badge>
                <span>From Chief of Staff · 2h ago</span>
              </>
            }
            actions={
              <>
                <Button size="sm">Deny</Button>
                <Button size="sm" variant="primary">Approve</Button>
              </>
            }
          >
            A research agent to track seed suppliers and summarize new catalogues weekly.
          </Card>
          <Card title="Weekly supplier summary" meta="Done yesterday" collapsible defaultOpen={false}>
            Three suppliers changed pricing; details in the attached report.
          </Card>
        </div>
      </Component>

      <Component name="Banner">
        <div className="app-col app-narrow">
          <Banner tone="info" title="Chief of Staff is rotating its chat.">New messages will wait until it is back.</Banner>
          <Banner tone="warning" title="3 messages need you." action={<Button size="sm">Review</Button>} />
          <Banner tone="danger" title="Couldn't reach the model provider." onDismiss={() => {}}>Retrying in 5 minutes.</Banner>
          <Banner tone="success" title="Backup restored." />
        </div>
      </Component>

      <Component name="EmptyState">
        <EmptyState title="No agents yet" action={<Button variant="primary">Hire your first agent</Button>}>
          Every team starts with one. Describe the role and Kivali drafts the brief.
        </EmptyState>
      </Component>

      <Component name="Dialog">
        <DialogDemo />
      </Component>

      <Component name="Tabs">
        <Tabs
          tabs={[
            { value: 'chat', label: 'Chat', content: <p className="app-inline-text">The agent's current conversation.</p> },
            { value: 'memory', label: 'Memory', count: 214, content: <p className="app-inline-text">What it remembers.</p> },
            { value: 'role', label: 'Role', content: <p className="app-inline-text">Its brief.</p> },
            { value: 'past', label: 'Past chats', count: 6, content: <p className="app-inline-text">Earlier conversations.</p> },
          ]}
        />
      </Component>

      <Component name="Menu">
        <div className="app-row">
          <Menu
            align="start"
            trigger={<Button variant="ghost" iconOnly icon={<Icon name="ellipsis" />} aria-label="Agent actions" />}
            items={[
              { icon: 'message-square', label: 'Start new chat' },
              { icon: 'pencil', label: 'Edit role' },
              { icon: 'copy', label: 'Copy agent ID', shortcut: '⌘C' },
              { icon: 'key', label: 'Rotate keys', disabled: true },
              { separator: true },
              { icon: 'archive', label: 'Offboard', danger: true },
            ]}
          />
          <Menu
            side="top"
            align="start"
            trigger={<Button>Opens upward</Button>}
            items={[{ icon: 'sun', label: 'Theme' }, { separator: true }, { icon: 'log-out', label: 'Sign out' }]}
          />
        </div>
      </Component>

      <Component name="Tooltip">
        <div className="app-tooltip-pad">
          <Tooltip content="Release all queued messages">
            <Button iconOnly icon={<Icon name="send" />} aria-label="Release all" />
          </Tooltip>
        </div>
      </Component>

      <Component name="Toast">
        <div className="app-col">
          <Toast tone="success" title="Skill installed" onDismiss={() => {}}>Research is available to every agent.</Toast>
          <Toast tone="info" title="3 messages released" action={<Button size="sm" variant="ghost">Undo</Button>} />
          <Toast tone="danger" title="Couldn't save the role" onDismiss={() => {}}>Your changes are still in the editor.</Toast>
        </div>
      </Component>

      <Component name="ListRow">
        <div className="app-box app-narrow">
          <ListRow onClick={() => {}} lead={<AgentAvatar name="Chief of Staff" role="compass" color="iris" size={32} />} title="Chief of Staff" meta={<><AgentState state="running" /><span>Reports to you</span></>} trail="2m ago" />
          <ListRow onClick={() => {}} selected lead={<AgentAvatar name="Garden advisor" role="sprout" color="olive" size={32} />} title="Garden advisor" meta={<><AgentState state="idle" /><span>Reports to Chief of Staff</span></>} trail="1h ago" />
          <ListRow onClick={() => {}} lead={<Icon name="file-text" size={20} />} title="Seed catalogue.pdf" meta="Project file · 2.4 MB" trail={<Badge variant="outline" mono>v3</Badge>} />
        </div>
      </Component>

      <Component name="Table">
        <Table
          columns={[
            { key: 'name', label: 'File' },
            { key: 'kind', label: 'Kind' },
            { key: 'size', label: 'Size', align: 'right', mono: true },
            { key: 'when', label: 'Added', mono: true },
          ]}
          rows={TABLE_ROWS}
        />
      </Component>

      <Component name="FileDrop">
        <div className="app-narrow">
          <FileDrop label="Drop project files here" hint="PDF, Markdown, images or ZIP, up to 25 MB each" />
        </div>
      </Component>

      <Component name="Progress">
        <div className="app-col app-narrow">
          <Progress label="Uploading 3 files" value={62} />
          <Progress label="Monthly budget used" value={81} tone="cobalt" />
          <Progress label="Setup" value={100} tone="success" />
        </div>
      </Component>

      <Component name="Skeleton">
        <div className="app-row app-narrow">
          <Skeleton width={32} height={32} />
          <Skeleton width="60%" />
          <Skeleton width={32} height={32} round />
        </div>
      </Component>

      <Component name="Prose">
        <Prose
          html={
            "<h2>Garden advisor</h2><p>Checks <strong>the forecast</strong> each morning and flags anything unusual before you water. Reads the <a href='#'>weather feed</a> and your garden notes.</p><ul><li>Waters the raised beds after two dry days</li><li>Reports weekly on what it learned</li></ul><blockquote>Ask before pruning anything older than three years.</blockquote><p>Runs <code>check_weather</code> at 6am.</p>"
          }
        />
      </Component>

      <Component name="CodeBlock">
        <div className="app-narrow">
          <CodeBlock title="Terminal" code={'kivali hire --role gardener \\\n  --reports-to chief-of-staff'} />
        </div>
      </Component>

      <Component name="AutoRelease">
        <div className="app-col">
          <AutoReleaseDemo initial="30s" variant={phone ? 'select' : 'slider'} />
          <AutoReleaseDemo initial="Off" variant={phone ? 'select' : 'slider'} />
          <Spec label='variant="select" (phone)'>
            <AutoReleaseDemo initial="2m" variant="select" />
          </Spec>
          <Text as="p" variant="label" tone="muted">Stops: {AUTO_RELEASE_STOPS.join(', ')}</Text>
        </div>
      </Component>

      <Component name="Readouts">
        <div className="app-col">
          <Readouts
            items={[
              { n: 3, label: 'blocked', href: '#' },
              { n: 12, label: 'running', href: '#' },
              { n: 5, label: 'queued', href: '#' },
              { n: 41, label: 'done this week', href: '#' },
            ]}
          />
          <Readouts items={[{ n: '$248', label: 'spent this month' }, { n: '82%', label: 'of budget' }]} />
        </div>
      </Component>
    </Group>
  );
}

function Identity() {
  return (
    <Group title="Identity and navigation">
      <Component name="AgentAvatar">
        <Spec label="Sizes, the role icon shows at 32 pixels and up">
          <div className="app-row">
            {[56, 40, 32, 24, 20, 16].map((s) => (
              <AgentAvatar key={s} name="Garden advisor" role="sprout" color="olive" size={s} />
            ))}
          </div>
        </Spec>
        <Spec label="A team, next to a person">
          <div className="app-row">
            {[
              ['Chief of Staff', 'compass', 'iris'],
              ['Garden advisor', 'sprout', 'olive'],
              ['Supplier scout', 'search', 'clay'],
              ['Policy advisor', 'scale', 'lake'],
              ['Bookkeeper', 'calculator', 'ochre'],
              ['Recruiter', 'user-search', 'plum'],
            ].map(([n, r, c]) => (
              <AgentAvatar key={n} name={n ?? ''} role={r} color={c} size={40} />
            ))}
            <PersonAvatar name="Maya Chen" size={40} />
          </div>
        </Spec>
        <Spec label="Identity colors">
          <div className="app-row">
            {identityColors.map((c) => (
              <div className="app-icon-cell" key={c}>
                <AgentAvatar name={c} initials={c.slice(0, 2).toUpperCase()} color={c} size={40} role="user" />
                <span className="app-icon-name">{c}</span>
              </div>
            ))}
          </div>
        </Spec>
        <Spec label={'Derived color for "' + 'Bookkeeper' + '": ' + colorFor('Bookkeeper')}>
          <div className="app-row">
            <AgentAvatar name="Bookkeeper" role="calculator" size={40} />
          </div>
        </Spec>
        <Spec label={'Role icons, ' + roleIconNames.length + ' in the set'}>
          <div className="app-icon-grid">
            {roleIconNames.map((r) => (
              <div className="app-icon-cell" key={r}>
                <AgentAvatar name={r} initials="Ab" role={r} color="slate" size={40} />
                <span className="app-icon-name">{r}</span>
              </div>
            ))}
          </div>
        </Spec>
      </Component>

      <Component name="PersonAvatar">
        <div className="app-row">
          {[56, 40, 32, 24, 20, 16].map((s) => (
            <PersonAvatar key={s} name="Maya Chen" size={s} />
          ))}
        </div>
      </Component>

      <Component name="OrgMark">
        <Spec label="With an uploaded logo">
          <div className="app-row">
            {[40, 28, 20].map((s) => (
              <OrgMark key={s} name="Sunset Gardens" src={LOGO} size={s} />
            ))}
          </div>
        </Spec>
        <Spec label="Fallback, initials on the identity color">
          <div className="app-row">
            {[40, 28, 20].map((s) => (
              <OrgMark key={s} name="Juniper Studio" color="clay" size={s} />
            ))}
            <OrgMark name="Personal" color="lake" size={40} />
            <OrgMark name="Kivali Labs" color="iris" size={40} />
          </div>
        </Spec>
      </Component>

      <Component name="NavItem">
        <div className="app-sidebar-demo">
          <NavItem icon="inbox" label="Inbox" count={3} attention active />
          <NavItem icon="list-todo" label="Work" count={12} />
          <NavItem icon="network" label="Graph" />
          <NavItem icon="settings" label="Settings" />
          <NavItem lead={<AgentAvatar name="Chief of Staff" role="compass" color="iris" size={20} />} label="Chief of Staff" count={<><AgentState state="running" compact /><ContextCount value={86} /></>} />
          <NavItem lead={<AgentAvatar name="Garden advisor" role="sprout" color="olive" size={20} />} label="Garden advisor" depth={1} />
        </div>
      </Component>

      <Component name="AgentState">
        <Spec label="Full, the default">
          <div className="app-state-grid">
            {RUN_STATES.map((s) => (
              <AgentState key={s} state={s} />
            ))}
          </div>
        </Spec>
        <Spec label="Compact, dense lists and the org tree">
          <div className="app-row">
            {RUN_STATES.map((s) => (
              <AgentState key={s} state={s} compact />
            ))}
          </div>
        </Spec>
      </Component>
    </Group>
  );
}

function Chat({ phone }: { phone: boolean }) {
  const person = { kind: 'person' as const, name: 'Maya Chen' };
  const agent = { kind: 'agent' as const, ...GA };
  return (
    <Group title="Chat">
      <Component name="Message">
        <div className="app-col app-narrow">
          <Message from={person} time="6:02">Should I water the tomatoes today?</Message>
          <Message from={agent} time="6:02" model="opus · high">
            <Prose>
              <p>
                Not today. The beds were watered on Friday, and <strong>6mm of rain</strong> is forecast for this afternoon.
              </p>
            </Prose>
          </Message>
          <Message from={{ kind: 'system' }} time="6:30">Chat rotated · memory saved</Message>
          <Message from={agent} time="6:31" streaming>I'll check again tomorrow at 6am and</Message>
          <Message from={person} time="6:31" pending onDelete={() => {}} onSendNow={() => {}}>
            Also check bed 3 while you're at it.
          </Message>
        </div>
      </Component>

      <Component name="Thinking">
        <div className="app-col app-narrow">
          <Thinking active />
          <Thinking active={false} seconds={12} defaultOpen>
            The beds were watered recently and rain is forecast; watering would waste water and risk root rot.
          </Thinking>
        </div>
      </Component>

      <Component name="ToolCall">
        <div className="app-col-tight app-narrow">
          <ToolCall name="garden_notes" summary="bed 3, last watered" status="running" defaultOpen input={{ bed: 3, note: 'watering' }} />
          <ToolCall name="weather_forecast" summary="next 24h, Outer Sunset" status="done" duration="0.8s" defaultOpen input={{ location: 'Outer Sunset', hours: 24 }} output="6mm rain expected 14:00 to 17:00" />
          <ToolCall name="send_message" summary="to Chief of Staff" status="error" duration="2.1s" input={{ to: 'chief-of-staff' }} error="Recipient is rotating its chat. Message queued." />
        </div>
      </Component>

      <Component name="SubagentTask">
        <div className="app-col app-narrow">
          <Spec label="Running, shown opened">
            <SubagentTask title="Compare three seed suppliers" state="running" defaultOpen model="sonnet" effort="medium" elapsed="1m 12s" latest="Reading the co-op catalogue, page 4 of 9" sessionHref="#">
              <ToolCall name="web_search" summary="heirloom tomato seed prices" status="done" duration="1.2s" />
              <ToolCall name="read_file" summary="coop-catalogue.pdf" status="running" defaultOpen input={{ path: 'supplier-docs/coop-catalogue.pdf', pages: '4-9' }} />
            </SubagentTask>
          </Spec>
          <Spec label="Done, shown opened">
            <SubagentTask
              title="Summarize last week's rainfall"
              state="done"
              model="haiku"
              effort="low"
              elapsed="18s"
              defaultOpen
              result="Total rainfall was 14mm across three days, most of it Tuesday afternoon. The east beds stayed damp for four days afterward, so no watering was needed. The west beds dried faster and needed water by Saturday morning, as we agreed."
              sessionHref="#"
            />
          </Spec>
          <Spec label="Failed, as it first appears, collapsed">
            <SubagentTask title="Pull invoices from the supplier portal" state="errored" model="sonnet" effort="high" elapsed="42s" error="Login rejected: the portal asked for a one-time code." sessionHref="#" />
          </Spec>
        </div>
      </Component>

      <Component name="SubagentGroup">
        <div className="app-narrow">
          <SubagentGroup
            defaultOpen
            tasks={[
              { title: 'Compare three seed suppliers', state: 'running', model: 'sonnet', effort: 'medium', elapsed: '1m 12s', latest: 'Reading the co-op catalogue, page 4 of 9', defaultOpen: true },
              { title: "Summarize last week's rainfall", state: 'done', model: 'haiku', effort: 'low', elapsed: '18s', result: 'Total rainfall was 14mm across three days.' },
              { title: 'Check the summer watering rules', state: 'done', model: 'sonnet', effort: 'low', elapsed: '31s', result: 'No watering limits announced for 2026.' },
            ]}
          />
        </div>
      </Component>

      <Component name="Composer">
        <div className="app-narrow">
          <Composer
            placeholder="Message Garden advisor"
            onAttach={() => {}}
            footer={
              <>
                <Badge variant="outline" mono>sonnet · high</Badge>
                <Badge variant="outline">1 file</Badge>
              </>
            }
          />
        </div>
        <Spec label="Busy and disabled">
          <div className="app-col app-narrow">
            <Composer placeholder="Message Garden advisor" busy defaultValue="Working on it" />
            <Composer placeholder="Message Garden advisor" disabled />
          </div>
        </Spec>
      </Component>

      <Component name="ContextGauge">
        <div className="app-col">
          <ContextGauge value={32} onNewChat={() => {}} compact={phone} />
          <ContextGauge value={64} onNewChat={() => {}} compact={phone} />
          <ContextGauge value={86} onNewChat={() => {}} compact={phone} />
          <div className="app-row">
            <Text variant="label" tone="muted">ContextCount</Text>
            <ContextCount value={91} />
            <Text variant="label" tone="muted">(hidden below the threshold)</Text>
          </div>
        </div>
      </Component>

      <Component name="ChatPager">
        <div className="app-col app-narrow">
          <ChatPagerDemo compact={phone} />
          {!phone && <ChatPagerDemo compact />}
        </div>
      </Component>
    </Group>
  );
}

function Work({ phone }: { phone: boolean }) {
  return (
    <Group title="Work">
      <Component name="AssignmentState">
        <div className="app-col">
          <div className="app-row">
            {ASSIGNMENT_STATES.map((s) => (
              <AssignmentState key={s} state={s} />
            ))}
          </div>
          <div className="app-row">
            {ASSIGNMENT_STATES.map((s) => (
              <AssignmentState key={s} state={s} compact />
            ))}
          </div>
        </div>
      </Component>

      <Component name="AssignmentRef">
        <p className="app-inline-text app-narrow">
          Waiting on <AssignmentRef id={12} state="ready" href="#" /> and{' '}
          <AssignmentRef id={15} state="blocked" href="#" title="Price out three seed suppliers" />; closes <AssignmentRef id={7} state="done" href="#" />.
        </p>
      </Component>

      <Component name="AcceptanceMeter">
        <div className="app-col">
          <AcceptanceMeter satisfied={3} claimed={1} unclaimed={1} />
          <AcceptanceMeter satisfied={4} claimed={0} unclaimed={0} />
          <div className="app-row">
            <Text variant="label" tone="muted">Compact</Text>
            <AcceptanceMeter satisfied={1} claimed={2} unclaimed={2} compact />
          </div>
        </div>
      </Component>

      <Component name="AssignmentRow">
        <div className="app-box app-narrow">
          <AssignmentRow id={7} title="Choose a seed supplier for the spring beds" state="blocked" assignee={CS} expanded onToggle={() => {}} onClick={() => {}} openChildren={2} acceptance={{ satisfied: 1, claimed: 1, unclaimed: 1 }} />
          <AssignmentRow id={12} title="Price out three seed suppliers" state="ready" assignee={SS} depth={1} onClick={() => {}} />
          <AssignmentRow id={15} title="Check the budget for tools" state="blocked" assignee={BK} depth={1} onClick={() => {}} waitingOn={[{ id: 12, state: 'ready' }]} />
          <AssignmentRow id={18} title="Approve spend over $500" state="held" assignee={{ kind: 'person', name: 'Maya Chen' }} depth={1} last onClick={() => {}} heldBy="you" selected />
          <AssignmentRow id={4} title="Draft the planting calendar" state="done" assignee={CS} onClick={() => {}} />
        </div>
      </Component>

      <Component name="NodeChip">
        <div className="app-col">
          <NodeChip id="decision/seed-supplier" kind="decision" summary="Buy from the local seed co-op, not the big chain" owner="iris" ownerName="Chief of Staff" href="#" />
          <NodeChip id="req/seed-budget" kind="requirement" summary="Seeds under $50 a season" owner="ochre" ownerName="Bookkeeper" href="#" selected />
          <NodeChip id="garden-plan-2026.pdf" kind="artifact" summary="Project file · v3" href="#" />
          <div className="app-row">
            <NodeChip id="decision/chain-store-seeds" kind="decision" status="superseded" href="#" />
            <NodeChip id="req/watering-rules" kind="requirement" status="flagged" href="#" />
            <NodeChip id="decision/drip-line" kind="decision" status="unresolved" href="#" />
          </div>
        </div>
      </Component>

      <Component name="GoalRow">
        <div className="app-box app-narrow">
          <GoalRow
            title="Choose a seed supplier for the spring beds"
            href="#"
            owner={CS}
            done={3}
            total={7}
            blocker="Waiting on vendor quotes"
            maxWorkers={phone ? 2 : 3}
            workers={[
              { ...SS, state: 'running' },
              { ...BK, state: 'blocked' },
              { ...GA, state: 'running' },
              { name: 'Weather watcher', state: 'queued' },
            ]}
          />
          <GoalRow title="Draft the planting calendar" href="#" owner={GA} done={5} total={5} workers={[]} />
        </div>
      </Component>

      <Component name="QueueRow">
        <QueueDemo />
      </Component>

      <Component name="DocDiff">
        <Spec label="Before and after">
          <div className="app-narrow">
            <DocDiff before={DIFF_BEFORE} after={DIFF_AFTER} />
          </div>
        </Spec>
        <Spec label="A new document">
          <div className="app-narrow">
            <DocDiff after={DIFF_AFTER} />
          </div>
        </Spec>
      </Component>

      <Component name="NodeRow">
        <NodeRowDemo compact={phone} />
      </Component>
    </Group>
  );
}

function Charts({ phone }: { phone: boolean }) {
  return (
    <Group title="Charts">
      <Component name="RankedBars">
        <div className="app-narrow">
          <RankedBars
            items={[
              { label: 'Chief of Staff', value: 84 },
              { label: 'Supplier scout', value: 61 },
              { label: 'Garden advisor', value: 38 },
              { label: 'Bookkeeper', value: 22 },
              { label: 'Weather watcher', value: 14 },
              { label: 'Pest checker', value: 6 },
              { label: 'Seed librarian', value: 4 },
              { label: 'Mail sorter', value: 3 },
            ]}
          />
        </div>
      </Component>

      <Component name="DayBars">
        <DayBars
          values={SPEND}
          width={phone ? 324 : 660}
          labels={[
            [0, 'Sep 1'],
            [14, 'Sep 15'],
            [29, 'Sep 30'],
          ]}
          ariaLabel="Spend per day, September"
        />
      </Component>
    </Group>
  );
}

const THEME_OPTIONS = [
  { value: 'device', label: 'Match device' },
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
];

const GROUPS = ['Core', 'Identity and navigation', 'Chat', 'Work', 'Charts'];

/** Every design system component in the states its specimen card shows. Route: /_ds. */
export function Gallery() {
  const [theme, setTheme] = useState<ThemeChoice>(getStoredTheme);
  const [phone, setPhone] = useState(false);
  return (
    <TooltipProvider>
      <div className="app-gallery">
        <header className="app-gallery-bar">
          <div className="app-gallery-bar-title">
            <Text as="h1" variant="heading">Kivali design system</Text>
            <Text as="p" variant="label" tone="muted">{GROUPS.join(' · ')}</Text>
          </div>
          <div className="app-gallery-controls">
            <Select
              label="Theme"
              value={theme}
              options={THEME_OPTIONS}
              onChange={(e) => {
                const next = e.target.value as ThemeChoice;
                setTheme(next);
                applyTheme(next);
              }}
            />
            <Switch label="Phone width" checked={phone} onCheckedChange={setPhone} />
          </div>
        </header>
        <main className={phone ? 'app-gallery-column is-phone' : 'app-gallery-column'}>
          <Core phone={phone} />
          <Identity />
          <Chat phone={phone} />
          <Work phone={phone} />
          <Charts phone={phone} />
        </main>
      </div>
    </TooltipProvider>
  );
}
