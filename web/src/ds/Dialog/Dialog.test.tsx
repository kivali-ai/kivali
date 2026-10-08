import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Button } from '../Button/Button';
import { Dialog, DialogClose } from './Dialog';

function Sample({ onOpenChange }: { onOpenChange?: (o: boolean) => void }) {
  return (
    <Dialog
      trigger={<Button>Offboard</Button>}
      tone="danger"
      title="Offboard Garden advisor?"
      description="Its files move to the archive."
      onOpenChange={onOpenChange}
      footer={
        <>
          <DialogClose asChild>
            <Button>Cancel</Button>
          </DialogClose>
          <Button variant="danger">Confirm offboard</Button>
        </>
      }
    />
  );
}

describe('Dialog', () => {
  it('is closed until the trigger is used, then opens as a labelled modal', async () => {
    render(<Sample />);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Offboard' }));
    const dialog = screen.getByRole('dialog', { name: 'Offboard Garden advisor?' });
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(dialog).toHaveAccessibleDescription('Its files move to the archive.');
    expect(screen.getByRole('button', { name: 'Confirm offboard' })).toBeInTheDocument();
  });

  it('closes on Escape', async () => {
    const onOpenChange = vi.fn();
    render(<Sample onOpenChange={onOpenChange} />);
    await userEvent.click(screen.getByRole('button', { name: 'Offboard' }));
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
  });

  it('closes from DialogClose and from the close button', async () => {
    render(<Sample />);
    await userEvent.click(screen.getByRole('button', { name: 'Offboard' }));
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Offboard' }));
    await userEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('can be controlled with open', () => {
    render(
      <Dialog open title="Rename agent">
        <p>Body text</p>
      </Dialog>,
    );
    expect(screen.getByRole('dialog', { name: 'Rename agent' })).toBeInTheDocument();
    expect(screen.getByText('Body text')).toBeInTheDocument();
  });
});
