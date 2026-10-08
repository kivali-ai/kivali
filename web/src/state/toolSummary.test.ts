import { describe, expect, it } from 'vitest';
import { bareToolName, summarizeTool } from './toolSummary';

describe('toolSummary', () => {
  it('summarises an assignment tool the same with or without the server prefix', () => {
    const input = '{"title":"Price the hosting","assignee":"buyer"}';
    const want = 'Opening an assignment for buyer: Price the hosting';
    expect(summarizeTool('mcp__kivali__assignment_create', input)).toBe(want);
    expect(summarizeTool('assignment_create', input)).toBe(want);
    expect(summarizeTool('assignment_view', { id: 42 })).toBe('Reading #42');
  });

  it('summarises a habits tool', () => {
    expect(summarizeTool('agent_memory_habits_append', '{"text":"x"}')).toBe('Appending to habits');
  });

  it('summarises the handbook tools', () => {
    expect(summarizeTool('mcp__kivali__read_handbook', '{}')).toBe('Reading the handbook');
    expect(summarizeTool('read_handbook', { section: 'How the org works' })).toBe('Reading the handbook: How the org works');
    expect(summarizeTool('propose_handbook_update', { title: 'x' })).toBe('Proposing a handbook change');
  });

  it('strips only the MCP server prefix', () => {
    expect(bareToolName('mcp__kivali__assignment_list')).toBe('assignment_list');
    expect(bareToolName('file_view')).toBe('file_view');
  });
});
