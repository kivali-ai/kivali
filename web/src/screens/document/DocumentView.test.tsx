import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderApp } from '../../app/test-utils';
import { documentBack, documentPath } from '../../app/attachments';
import { goSnapshot } from '../../state/fixtures';
import { DOCUMENT_MAX_CHARS } from './DocumentView';

afterEach(() => {
  vi.unstubAllGlobals();
});

function text(body: string): Response {
  return new Response(body, { status: 200, headers: { 'Content-Type': 'application/octet-stream' } });
}

describe('DocumentView', () => {
  // Guards against a markdown attachment being shown as raw text, or only offered as a download: the page formats
  // it as a message body is, inside the app's frame, with the file name and a Download button.
  it('formats a markdown attachment with its name and a Download button', async () => {
    const { calls } = renderApp('/documents/aa?name=plan.md', goSnapshot, undefined, {
      'GET /attachments/aa': () => text('# Launch plan\n\nShip **on Friday**.\n\n- one\n- two\n'),
    });
    expect(await screen.findByRole('heading', { name: 'Launch plan' })).toBeInTheDocument();
    expect(screen.getByText('on Friday').tagName).toBe('STRONG');
    // The frame's phone header carries the title too; CSS shows one of the two at each width.
    expect(screen.getAllByRole('heading', { level: 1, name: 'plan.md' }).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Download' })).toBeInTheDocument();
    expect(calls.some((c) => c.url === '/attachments/aa')).toBe(true);
  });

  // Guards against agent-written markdown running script in the app: the text goes through the same sanitizer as
  // chat messages.
  it('drops script from the document', async () => {
    renderApp('/documents/aa?name=x.md', goSnapshot, undefined, {
      'GET /attachments/aa': () => text('# Title\n\n<script>window.__ran = true</script><img src=x onerror="window.__ran = true">\n'),
    });
    const heading = await screen.findByRole('heading', { name: 'Title' });
    const body = heading.closest('.app-document-body') as HTMLElement;
    expect(body.querySelector('script')).toBeNull();
    expect(body.innerHTML).not.toContain('onerror');
  });

  // Guards against a missing file leaving a blank page: it says so, with a way to try again.
  it('says when the file is not there', async () => {
    renderApp('/documents/gone?name=x.md', goSnapshot);
    expect(await screen.findByText('This file isn’t here.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  // Guards against freezing the page formatting a huge file: past the limit it is offered as a download only.
  it('offers a very large file as a download only', async () => {
    renderApp('/documents/aa?name=big.md', goSnapshot, undefined, {
      'GET /attachments/aa': () => text('# Big\n' + 'x'.repeat(DOCUMENT_MAX_CHARS)),
    });
    expect(await screen.findByText('This file is too large to show here. Download it to read it.')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Big' })).toBeNull();
  });

  // Guards against Back being a dead end when the page was opened without a known origin (a reload, a pasted link).
  it('leaves for Home from Back when it was not opened from a page', async () => {
    renderApp('/documents/aa?name=plan.md', goSnapshot, undefined, { 'GET /attachments/aa': () => text('# Plan\n') });
    await screen.findByRole('heading', { name: 'Plan' });
    await userEvent.click(screen.getAllByRole('button', { name: 'Back' })[0] as HTMLElement);
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Plan' })).toBeNull());
    expect(screen.queryByRole('button', { name: 'Download' })).toBeNull();
  });
});

describe('attachment documents', () => {
  // Guards against Back sending the person off the app: only a same-origin path from the opener's state is used.
  it('returns only to an in-app path', () => {
    expect(documentBack({ from: '/agents/a?x=1' })).toBe('/agents/a?x=1');
    expect(documentBack({ from: '//elsewhere.example/' })).toBe('/');
    expect(documentBack({ from: 'https://elsewhere.example/' })).toBe('/');
    expect(documentBack(null)).toBe('/');
  });

  // Guards against a document page for something that is not a stored attachment (a web task's file, say).
  it('makes a document page only for a stored attachment', () => {
    expect(documentPath({ name: 'a b.md', size_bytes: 0, url: '/attachments/3f2a' })).toBe('/documents/3f2a?name=a%20b.md');
    expect(documentPath({ name: 'a.md', size_bytes: 0, url: '/api/v1/agents/x/web-files/a.md' })).toBeNull();
  });
});
