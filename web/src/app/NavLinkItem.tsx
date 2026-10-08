import { useHref } from 'react-router';
import { NavItem } from '../ds';
import type { NavItemProps } from '../ds';

/** A NavItem that points at a router path. It renders a real link; `useInternalLinkInterception` makes it a soft navigation. */
export function NavLinkItem({ to, ...rest }: Omit<NavItemProps, 'href' | 'onClick'> & { to: string }) {
  const href = useHref(to);
  return <NavItem {...rest} href={href} />;
}

/**
 * A comma only screen readers hear. A NavItem's name is its text run together ("Engineering leadWorking86%"),
 * so the frame puts one before each extra part: "Engineering lead, Working, 86%".
 */
export function NameSep() {
  return <span className="app-sr-only">, </span>;
}
