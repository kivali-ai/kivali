import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import { Gallery } from './Gallery';

const COMPONENTS = [
  'AcceptanceMeter', 'AgentAvatar', 'AgentState', 'AutoRelease', 'Badge', 'Banner', 'Button', 'Card', 'ChatPager', 'Checkbox',
  'CodeBlock', 'Composer', 'ContextGauge', 'DayBars', 'Dialog', 'DocDiff', 'EmptyState', 'FileDrop', 'GoalRow', 'Icon',
  'AssignmentRef', 'AssignmentRow', 'AssignmentState', 'ListRow', 'Menu', 'Message', 'NavItem', 'NodeChip', 'NodeRow', 'OrgMark',
  'PersonAvatar', 'Progress', 'Prose', 'QueueRow', 'RankedBars', 'Readouts', 'Select', 'Skeleton',
  'SubagentGroup', 'SubagentTask', 'Switch', 'Table', 'Tabs', 'TextField', 'Thinking', 'Toast', 'ToolCall', 'Tooltip',
  // Kivali addition.
  'Text',
];

describe('Gallery', () => {
  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
  });

  it('has a section for every ported component folder (48) plus Text', () => {
    expect(COMPONENTS).toHaveLength(49);
    render(<Gallery />);
    for (const name of COMPONENTS) {
      expect(screen.getByRole('heading', { level: 3, name }), name).toBeInTheDocument();
    }
  });

  it('groups them under the five group headings', () => {
    render(<Gallery />);
    const groups = Array.from(document.querySelectorAll('.app-group-title')).map((h) => h.textContent);
    expect(groups).toEqual(['Core', 'Identity and navigation', 'Chat', 'Work', 'Charts']);
  });

  it('switches the theme', async () => {
    render(<Gallery />);
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Theme' }), 'dark');
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Theme' }), 'light');
    expect(document.documentElement).not.toHaveAttribute('data-theme');
  });

  it('constrains the column to phone width', async () => {
    const { container } = render(<Gallery />);
    const column = container.querySelector('main');
    expect(column).not.toHaveClass('is-phone');
    await userEvent.click(screen.getByRole('switch', { name: 'Phone width' }));
    expect(column).toHaveClass('is-phone');
  });
});
