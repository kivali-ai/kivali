import { describe, expect, it } from 'vitest';
import { markdownBlocks, renderMarkdown, safeHref, sanitizeHtml } from './markdown';

describe('renderMarkdown', () => {
  it('renders formatting and lists', () => {
    const html = renderMarkdown('Hello **there**\n\n- one\n- two');
    expect(html).toContain('<strong>there</strong>');
    expect(html).toContain('<li>one</li>');
  });

  it('is empty for blank input', () => {
    expect(renderMarkdown('  \n')).toBe('');
  });

  it('drops script, event handlers and styles from raw HTML', () => {
    const html = renderMarkdown('Hi <script>alert(1)</script><b onclick="x()" style="color:red">bold</b><img src=x onerror=y>');
    expect(html).not.toContain('script');
    expect(html).not.toContain('onclick');
    expect(html).not.toContain('style');
    expect(html).not.toContain('<img');
    expect(html).toContain('<b>bold</b>');
  });

  it('unwraps unknown elements but keeps their text', () => {
    expect(sanitizeHtml('<div><span class="x">kept</span></div>')).toBe('kept');
  });

  it('strips javascript: links and opens external links in a new tab', () => {
    const bad = renderMarkdown('[x](javascript:alert(1))');
    expect(bad).not.toContain('javascript');
    const ext = renderMarkdown('[site](https://example.com)');
    expect(ext).toContain('href="https://example.com"');
    expect(ext).toContain('rel="noopener noreferrer"');
    const rel = renderMarkdown('[assignment](/assignments/4)');
    expect(rel).toContain('href="/assignments/4"');
    expect(rel).not.toContain('target');
  });
});

describe('markdownBlocks', () => {
  // Guards the split a page uses to give code blocks a copy button: each top-level code block comes out whole, with its
  // info string as written, between the HTML around it, which is still sanitized and still resolves reference links
  // defined after the block. A code block inside a list stays in its HTML, and blank input gives nothing.
  it('splits at top-level code blocks and sanitizes the rest', () => {
    const blocks = markdownBlocks(
      'Open [the form][f] <script>x()</script>\n\n```Organization address\nhttps://github.com/a?b=1&c=2\n```\n\n1. one\n\n       indented in a list\n\n[f]: https://github.com/settings/apps/new\n',
    );
    expect(blocks.map((b) => b.kind)).toEqual(['html', 'code', 'html']);
    const [intro, code, rest] = blocks;
    expect(intro).toEqual({ kind: 'html', html: expect.stringContaining('href="https://github.com/settings/apps/new"') as string });
    expect(intro?.kind === 'html' && intro.html).not.toContain('script');
    expect(code).toEqual({ kind: 'code', code: 'https://github.com/a?b=1&c=2', info: 'Organization address' });
    expect(rest?.kind === 'html' && rest.html).toContain('<code>indented in a list');
    expect(markdownBlocks(' \n')).toEqual([]);
    expect(markdownBlocks('```\nonly\n```')).toEqual([{ kind: 'code', code: 'only', info: '' }]);
  });
});

describe('safeHref', () => {
  it('allows web, mail and relative links only', () => {
    expect(safeHref('https://a.b')).toBe(true);
    expect(safeHref('mailto:a@b.c')).toBe(true);
    expect(safeHref('/team')).toBe(true);
    expect(safeHref('#top')).toBe(true);
    expect(safeHref('java\nscript:alert(1)')).toBe(false);
    expect(safeHref('data:text/html,x')).toBe(false);
  });
});
