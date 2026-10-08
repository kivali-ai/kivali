import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { Button } from '../Button/Button';
import { Icon } from '../Icon/Icon';
import { Tooltip, TooltipProvider } from './Tooltip';

describe('Tooltip', () => {
  it('shows its content when the trigger receives focus', async () => {
    render(
      <TooltipProvider>
        <Tooltip content="Release all queued messages">
          <Button iconOnly icon={<Icon name="send" />} aria-label="Release all" />
        </Tooltip>
      </TooltipProvider>,
    );
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
    await userEvent.tab();
    expect(screen.getByRole('button', { name: 'Release all' })).toHaveFocus();
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Release all queued messages');
  });

  it('can be forced open', () => {
    render(
      <TooltipProvider>
        <Tooltip open content="Always shown">
          <Button>Trigger</Button>
        </Tooltip>
      </TooltipProvider>,
    );
    expect(screen.getByRole('tooltip')).toHaveTextContent('Always shown');
  });
});
