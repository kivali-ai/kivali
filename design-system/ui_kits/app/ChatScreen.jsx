const K_chat = window.KivaliDesignSystem_9b9983;

function ChatScreen({ agent }) {
  const { Message, Thinking, ToolCall, SubagentGroup, Composer, AgentAvatar, AgentState, Button, Icon, Badge, Tabs } = K_chat;
  const me = { kind: 'person', name: 'Maya Chen' };
  const ag = { kind: 'agent', name: agent.name, role: agent.role, color: agent.color };
  const [extra, setExtra] = React.useState([]);
  const [busy, setBusy] = React.useState(false);
  const send = (v) => {
    setExtra((x) => [...x, { from: me, text: v, time: 'now' }]);
    setBusy(true);
    setTimeout(() => {
      setExtra((x) => [...x, { from: ag, text: 'Noted. I’ll add that to what I remember about bed 3 and check it again tomorrow morning.', time: 'now', learned: true }]);
      setBusy(false);
    }, 1400);
  };
  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <header style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '14px var(--space-8)', borderBottom: '1px solid var(--line)' }}>
        <AgentAvatar name={agent.name} role={agent.role} color={agent.color} size={32} />
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ font: '500 17px/24px var(--font-display)', letterSpacing: '-.01em' }}>{agent.name}</div>
          <div style={{ display: 'flex', gap: 10, alignItems: 'center', font: 'var(--type-caption)', color: 'var(--ink-muted)' }}>
            <AgentState state={busy ? 'running' : agent.state} /><span>Memory · 214 notes</span>
          </div>
        </div>
        <Button variant="ghost" icon={<Icon name="book-open" />}>Brief</Button>
        <Button variant="ghost" iconOnly icon={<Icon name="ellipsis" />} aria-label="Agent actions" />
      </header>
      <div style={{ flex: 1, overflowY: 'auto' }}>
        <div style={{ maxWidth: 'var(--content)', margin: '0 auto', padding: 'var(--space-6) var(--space-8)', display: 'flex', flexDirection: 'column', gap: 'var(--space-6)' }}>
          <Message from={{ kind: 'system' }} time="09:02">Today</Message>
          <Message from={me} time="09:02">Should I water the tomatoes today?</Message>
          <Message from={ag} time="09:03">
            <Thinking active={false} seconds={4}>Bed 3 was last watered four days ago. Forecast says rain this afternoon. Check how much before recommending.</Thinking>
            <ToolCall name="garden_notes" summary="bed 3, last watered" status="done" duration="0.4s" input={{ bed: 3, note: 'watering' }} output="Watered Friday, four days ago" />
            <ToolCall name="weather_forecast" summary="next 24h, Outer Sunset" status="done" duration="0.8s" input={{ location: 'Outer Sunset', hours: 24 }} output="6mm rain expected 14:00 to 17:00" />
            <p style={{ margin: 0 }}>Hold off. Bed 3 is on the dry side, but about 6mm of rain is due between 2 and 5pm, which should be plenty. I’ll check again at 6pm and message you if it’s still dry.</p>
          </Message>
          <Message from={me} time="09:05">Can you also find a cheaper seed supplier for next spring?</Message>
          <Message from={ag} time="09:06">
            <SubagentGroup tasks={[
              { title: 'Compare three seed suppliers', state: 'running', model: 'sonnet', effort: 'medium', elapsed: '1m 12s', latest: 'Reading the co-op catalogue, page 4 of 9' },
              { title: 'Check the summer watering rules', state: 'done', model: 'haiku', effort: 'low', elapsed: '31s', result: 'No watering limits announced for 2026.' },
            ]} />
            <p style={{ margin: 0 }}>Two helpers are on it. I’ll bring back a short comparison with prices.</p>
          </Message>
          {extra.map((m, i) => (
            <Message key={i} from={m.from} time={m.time}>
              {m.text}
              {m.learned && <div><Badge tone="cobalt"><Icon name="brain" size={12} />Learned 1 thing</Badge></div>}
            </Message>
          ))}
          {busy && <Message from={ag} streaming><Thinking active /></Message>}
        </div>
      </div>
      <div style={{ maxWidth: 'var(--content)', width: '100%', boxSizing: 'border-box', margin: '0 auto', padding: '0 var(--space-8) var(--space-6)' }}>
        <Composer placeholder={'Message ' + agent.name} onSend={send} busy={busy} onAttach={() => {}}
          footer={<span style={{ font: 'var(--type-label)', color: 'var(--ink-muted)' }}>Enter to send · Shift+Enter for a new line</span>} />
      </div>
    </div>
  );
}
window.ChatScreen = ChatScreen;
