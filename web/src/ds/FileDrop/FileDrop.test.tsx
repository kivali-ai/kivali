import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { FileDrop } from './FileDrop';

describe('FileDrop', () => {
  it('renders the label, hint and picker prompt', () => {
    render(<FileDrop label="Drop project files here" hint="PDF, up to 25 MB" />);
    expect(screen.getByRole('button')).toHaveTextContent('Drop project files here');
    expect(screen.getByText('PDF, up to 25 MB')).toBeInTheDocument();
  });

  it('reports files chosen with the picker', async () => {
    const onFiles = vi.fn();
    const { container } = render(<FileDrop onFiles={onFiles} />);
    const file = new File(['x'], 'plan.md', { type: 'text/markdown' });
    await userEvent.upload(container.querySelector('input[type="file"]') as HTMLInputElement, file);
    expect(onFiles).toHaveBeenCalledWith([file]);
  });

  it('reports dropped files and tints while dragging over', () => {
    const onFiles = vi.fn();
    render(<FileDrop onFiles={onFiles} />);
    const zone = screen.getByRole('button');
    fireEvent.dragOver(zone);
    expect(zone).toHaveClass('is-over');
    const file = new File(['x'], 'plan.md');
    fireEvent.drop(zone, { dataTransfer: { files: [file] } });
    expect(zone).not.toHaveClass('is-over');
    expect(onFiles).toHaveBeenCalledWith([file]);
  });
});
