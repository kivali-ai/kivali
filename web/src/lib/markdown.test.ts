import { describe, expect, it } from 'vitest';
import { renderMarkdown, safeHref, sanitizeHtml } from './markdown';

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
