import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ToolCall } from './ToolCall';

describe('ToolCall', () => {
  it('shows the name, summary and duration when done', () => {
    render(<ToolCall name="read_log" summary="mail log, last hour" status="done" duration="0.4s" input={{ log: 'mail' }} output="6 bounced" />);
    expect(screen.getByText('read_log')).toBeInTheDocument();
    expect(screen.getByText('mail log, last hour')).toBeInTheDocument();
    expect(screen.getByText('0.4s')).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'Done' })).toBeInTheDocument();
  });

  it('formats object input as JSON and keeps text as is', () => {
    render(<ToolCall name="t" input={{ log: 'mail' }} output="6 bounced" />);
    expect(screen.getByText(/"log": "mail"/)).toBeInTheDocument();
    expect(screen.getByText('6 bounced')).toBeInTheDocument();
  });

  it('shows the error instead of the output and says Failed', () => {
    render(<ToolCall name="send_message" status="error" error="Recipient is rotating." output="ignored" />);
    expect(screen.getByRole('status', { name: 'Failed' })).toBeInTheDocument();
    expect(screen.getByText('Recipient is rotating.')).toBeInTheDocument();
    expect(screen.queryByText('ignored')).not.toBeInTheDocument();
  });

  it('says it is waiting for output while running and hides the duration', () => {
    render(<ToolCall name="t" status="running" duration="1s" input="x" />);
    expect(screen.getByText(/Waiting for output/, { selector: '.kv-tool-none' })).toBeInTheDocument();
    expect(screen.queryByText('1s')).not.toBeInTheDocument();
  });

  it('says so when there is no input or output', () => {
    render(<ToolCall name="t" />);
    expect(screen.getByText('No input')).toBeInTheDocument();
    expect(screen.getByText('No output')).toBeInTheDocument();
  });
});
