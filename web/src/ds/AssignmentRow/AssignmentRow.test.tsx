import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { AssignmentRow } from './AssignmentRow';

describe('AssignmentRow', () => {
  it('shows the title, id and state', () => {
    render(<AssignmentRow id={7} title="Choose a mail provider" state="blocked" />);
    expect(screen.getByText('Choose a mail provider')).toBeInTheDocument();
    expect(screen.getByText('#7')).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Blocked' })).toBeInTheDocument();
  });

  it('says what it is waiting on', () => {
    render(<AssignmentRow id={15} title="Check the budget" state="blocked" waitingOn={[{ id: 12, state: 'ready' }, { id: 13 }]} />);
    expect(screen.getByText(/Waiting on/)).toBeInTheDocument();
    expect(screen.getByText('#12')).toBeInTheDocument();
    expect(screen.getByText('#13')).toBeInTheDocument();
  });

  it('says what it waits on in words when the entries carry titles', () => {
    const { container } = render(<AssignmentRow id={42} title="Update the docs" state="blocked" waitingOn={[{ id: 45, state: 'ready', title: 'Run the tests' }]} />);
    expect(container.querySelector('.kv-irow-why')).toHaveTextContent(/^Waiting on #45, Run the tests$/);
  });

  it('separates titled entries with semicolons', () => {
    const { container } = render(
      <AssignmentRow
        id={42}
        title="Update the docs"
        state="blocked"
        waitingOn={[
          { id: 45, title: 'Run the tests' },
          { id: 46, title: 'Label' },
        ]}
      />,
    );
    expect(container.querySelector('.kv-irow-why')).toHaveTextContent(/^Waiting on #45, Run the tests; #46, Label$/);
  });

  it('says who holds it', () => {
    render(<AssignmentRow id={18} title="Approve spend" state="held" heldBy="you" />);
    expect(screen.getByText('On hold by you')).toBeInTheDocument();
  });

  it('shows the open children count, acceptance meter and assignee', () => {
    render(
      <AssignmentRow
        id={7}
        title="T"
        openChildren={2}
        acceptance={{ satisfied: 1, claimed: 1, unclaimed: 1 }}
        assignee={{ kind: 'person', name: 'Maya Chen' }}
      />,
    );
    expect(screen.getByTitle('Open children')).toHaveTextContent('2');
    expect(screen.getByRole('img', { name: '1 of 3 met · 1 in progress · 1 unclaimed' })).toBeInTheDocument();
    expect(screen.getByTitle('Maya Chen')).toHaveTextContent('MC');
  });

  it('renders an agent assignee as an avatar', () => {
    render(<AssignmentRow id={7} title="T" assignee={{ name: 'Chief of Staff', role: 'compass', color: 'iris' }} />);
    expect(screen.getByRole('img', { name: 'Chief of Staff' })).toBeInTheDocument();
  });

  it('toggles children without activating the row', async () => {
    const onToggle = vi.fn();
    const onClick = vi.fn();
    render(<AssignmentRow id={7} title="T" expanded onToggle={onToggle} onClick={onClick} />);
    await userEvent.click(screen.getByRole('button', { name: 'Collapse' }));
    expect(onToggle).toHaveBeenCalledTimes(1);
    expect(onClick).not.toHaveBeenCalled();
  });

  it('is a target with onClick and marks selection', async () => {
    const onClick = vi.fn();
    render(<AssignmentRow id={7} title="Pick me" onClick={onClick} selected />);
    const row = screen.getByRole('button', { name: /Pick me/ });
    expect(row).toHaveClass('is-selected');
    await userEvent.click(row);
    expect(onClick).toHaveBeenCalledTimes(1);
  });
});
