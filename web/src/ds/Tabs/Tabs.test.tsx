import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Tabs } from './Tabs';

const tabs = [
  { value: 'chat', label: 'Chat', content: <p>Current conversation.</p> },
  { value: 'memory', label: 'Memory', count: 214, content: <p>What it remembers.</p> },
  { value: 'role', label: 'Role' },
];

describe('Tabs', () => {
  it('shows the first tab by default with its panel', () => {
    render(<Tabs tabs={tabs} />);
    expect(screen.getByRole('tab', { name: 'Chat' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('tabpanel')).toHaveTextContent('Current conversation.');
  });

  it('switches tabs and reports the value', async () => {
    const onValueChange = vi.fn();
    render(<Tabs tabs={tabs} onValueChange={onValueChange} />);
    await userEvent.click(screen.getByRole('tab', { name: /Memory/ }));
    expect(screen.getByRole('tab', { name: /Memory/ })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('tab', { name: 'Chat' })).toHaveAttribute('aria-selected', 'false');
    expect(screen.getByRole('tabpanel')).toHaveTextContent('What it remembers.');
    expect(onValueChange).toHaveBeenCalledWith('memory');
  });

  it('renders the count badge next to the label', () => {
    render(<Tabs tabs={tabs} />);
    expect(within(screen.getByRole('tab', { name: /Memory/ })).getByText('214')).toBeInTheDocument();
  });

  it('moves between tabs with the arrow keys', async () => {
    render(<Tabs tabs={tabs} />);
    screen.getByRole('tab', { name: 'Chat' }).focus();
    await userEvent.keyboard('{ArrowRight}');
    expect(screen.getByRole('tab', { name: /Memory/ })).toHaveAttribute('aria-selected', 'true');
  });

  it('renders no panel for a tab without content', async () => {
    render(<Tabs tabs={tabs} defaultValue="role" />);
    expect(screen.getByRole('tab', { name: 'Role' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByRole('tabpanel')).not.toBeInTheDocument();
  });

  it('points aria-controls only at panels that exist', async () => {
    const mixed = [...tabs, { value: 'chats', label: 'Past chats' }];
    const check = () => {
      for (const tab of screen.getAllByRole('tab')) {
        const controls = tab.getAttribute('aria-controls');
        if (controls === null) continue;
        expect(document.getElementById(controls), `${tab.textContent} controls #${controls}`).not.toBeNull();
      }
    };
    render(<Tabs tabs={mixed} />);
    check();
    expect(screen.getByRole('tab', { name: 'Role' })).not.toHaveAttribute('aria-controls');
    expect(screen.getByRole('tab', { name: 'Past chats' })).not.toHaveAttribute('aria-controls');
    expect(screen.getByRole('tab', { name: 'Chat' })).toHaveAttribute('aria-controls');
    await userEvent.click(screen.getByRole('tab', { name: 'Role' }));
    check();
  });

  it('follows a controlled value', () => {
    render(<Tabs tabs={tabs} value="memory" />);
    expect(screen.getByRole('tab', { name: /Memory/ })).toHaveAttribute('aria-selected', 'true');
  });
});
