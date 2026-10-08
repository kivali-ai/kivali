import { Marked } from 'marked';

// Markdown that agents and people write, rendered for Prose. marked passes raw HTML through, so every render
// is sanitized against an allowlist: formatting tags stay, links keep safe schemes, and anything that can run
// code, load a resource or restyle the page is dropped.

const parser = new Marked({ gfm: true, breaks: false, async: false });

const ALLOWED = new Set([
  'a', 'b', 'blockquote', 'br', 'code', 'del', 'em', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'hr', 'i', 'li', 'ol',
  'p', 'pre', 's', 'strong', 'sub', 'sup', 'table', 'tbody', 'td', 'tfoot', 'th', 'thead', 'tr', 'ul',
]);

// Elements removed with everything inside them; any other unknown element is unwrapped (its text stays).
const DROPPED = new Set([
  'script', 'style', 'iframe', 'object', 'embed', 'template', 'svg', 'math', 'form', 'input', 'button', 'select',
  'textarea', 'noscript', 'link', 'meta', 'base', 'img', 'video', 'audio', 'source', 'picture', 'canvas', 'frame',
  'frameset', 'title', 'head',
]);

const ATTRS: Record<string, readonly string[]> = {
  a: ['href', 'title'],
  code: ['class'],
  td: ['align'],
  th: ['align'],
  ol: ['start'],
};

const SAFE_SCHEME = /^(?:https?|mailto):/i;
const ANY_SCHEME = /^[a-z][a-z0-9+.-]*:/i;

/** True for links that cannot run script: http(s), mailto, and scheme-less (relative, #fragment) URLs. */
export function safeHref(href: string): boolean {
  // Browsers ignore whitespace and control characters inside a scheme ("java\nscript:").
  const v = href.replace(/[\u0000- ]/g, '');
  if (SAFE_SCHEME.test(v)) return true;
  return !ANY_SCHEME.test(v);
}

function clean(parent: ParentNode): void {
  for (const node of Array.from(parent.childNodes)) {
    if (node.nodeType === Node.TEXT_NODE) continue;
    if (node.nodeType !== Node.ELEMENT_NODE) {
      node.remove();
      continue;
    }
    const el = node as Element;
    const tag = el.tagName.toLowerCase();
    if (DROPPED.has(tag)) {
      el.remove();
      continue;
    }
    clean(el);
    if (!ALLOWED.has(tag)) {
      el.replaceWith(...Array.from(el.childNodes));
      continue;
    }
    const keep = ATTRS[tag] ?? [];
    for (const attr of Array.from(el.attributes)) {
      if (!keep.includes(attr.name)) el.removeAttribute(attr.name);
    }
    if (tag === 'code') {
      const cls = el.getAttribute('class');
      if (cls && !/^language-[\w-]+$/.test(cls)) el.removeAttribute('class');
    }
    if (tag === 'a') {
      const href = el.getAttribute('href');
      if (href !== null && !safeHref(href)) el.removeAttribute('href');
      if (el.getAttribute('href') && ANY_SCHEME.test(el.getAttribute('href') ?? '')) {
        el.setAttribute('rel', 'noopener noreferrer');
        el.setAttribute('target', '_blank');
      }
    }
  }
}

/** Removes everything outside the allowlist from an HTML string. */
export function sanitizeHtml(html: string): string {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  clean(tpl.content);
  return tpl.innerHTML;
}

/** Markdown to sanitized HTML, ready for `<Prose html>`. */
export function renderMarkdown(src: string): string {
  if (!src.trim()) return '';
  return sanitizeHtml(parser.parse(src) as string);
}
