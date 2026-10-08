import { useEffect } from 'react';
import { useHref, useNavigate } from 'react-router';

/**
 * Paths the Go server answers itself rather than the app: the JSON API, raw messages, attachment downloads,
 * the org's uploaded logo, sign-in and sign-out, the build's own files, health and
 * version, and the sign-in screen (a full load, so the session
 * starts fresh). A link to one of them is a real navigation, never a router one. Keep in step with
 * wireRoutes in internal/web/web.go.
 */
const SERVER_PREFIXES = ['/api/', '/messages/', '/attachments/', '/branding/', '/auth/', '/assets/', '/logos/', '/admin/', '/debug/'];
const SERVER_PATHS = ['/favicon.ico', '/healthz', '/readyz', '/login'];
/** The SSE streams: /org/stream, /agents/{slug}/stream and /agents/{slug}/subagents/{id}/stream. */
const STREAM = /^\/(org|agents\/[^/]+(\/subagents\/[^/]+)?)\/stream$/;

/** Whether the server, not the app's router, answers `pathname`. */
export function serverOwned(pathname: string): boolean {
  return SERVER_PATHS.includes(pathname) || SERVER_PREFIXES.some((p) => pathname.startsWith(p)) || STREAM.test(pathname);
}

/** The router path (basename removed) for a same-origin href inside the app, or null when the link leaves it. */
export function internalPath(href: string, base: string, origin: string, current: string): string | null {
  let url: URL;
  try {
    url = new URL(href, current);
  } catch {
    return null;
  }
  if (url.origin !== origin) return null;
  if (serverOwned(url.pathname)) return null;
  const inside = base === '' || url.pathname === base || url.pathname.startsWith(base + '/');
  if (!inside) return null;
  const rest = url.pathname.slice(base.length) || '/';
  return rest + url.search + url.hash;
}

/**
 * The design system's NavItem, ListRow, GoalRow and AssignmentRow render plain `<a href>` (faithful to the reference),
 * so a click would reload the page. One delegated listener turns a plain click on a same-origin link inside the
 * app into a router navigation. Modified clicks, other buttons, `target`, `download`, and links that leave
 * the app's base path are left to the browser. Mount it once, inside the router.
 */
export function useInternalLinkInterception(): void {
  const navigate = useNavigate();
  // useHref('/') is the basename plus a slash; the app is mounted at the root, so this is ''.
  const base = useHref('/').replace(/\/$/, '');

  useEffect(() => {
    function onClick(ev: MouseEvent): void {
      if (ev.defaultPrevented || ev.button !== 0) return;
      if (ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
      const target = ev.target;
      if (!(target instanceof Element)) return;
      const anchor = target.closest('a');
      if (!anchor) return;
      if (anchor.target && anchor.target !== '_self') return;
      if (anchor.hasAttribute('download')) return;
      const href = anchor.getAttribute('href');
      if (!href || href.startsWith('#')) return;
      const path = internalPath(href, base, window.location.origin, window.location.href);
      if (path === null) return;
      ev.preventDefault();
      void navigate(path);
    }
    document.addEventListener('click', onClick);
    return () => document.removeEventListener('click', onClick);
  }, [base, navigate]);
}
