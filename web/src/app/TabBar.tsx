import { Link } from 'react-router';
import { Icon } from '../ds';
import { cx } from '../lib/cx';
import { DESTINATIONS } from './nav';
import type { DestinationKey } from './nav';

export interface TabBarProps {
  active: DestinationKey | null;
  needs: number;
}

/** Phone and tablet only (the stylesheet hides it from 960): the same five destinations, Home carrying the count. */
export function TabBar({ active, needs }: TabBarProps) {
  return (
    <nav className="app-tabbar" aria-label="Tab bar">
      {DESTINATIONS.map((d) => {
        const count = d.key === 'home' && needs > 0 ? needs : 0;
        return (
          <Link
            key={d.key}
            to={d.to}
            className={cx('app-tab', d.key === active && 'is-active')}
            aria-current={d.key === active ? 'page' : undefined}
            // Without it the name reads "3Home": the count sits before the word.
            aria-label={count ? d.label + ', ' + count + ' need you' : undefined}
          >
            <span className="app-tab-icon">
              <Icon name={d.icon} size={22} />
              {count > 0 && (
                <span className="app-tab-count" aria-hidden="true">
                  {count}
                </span>
              )}
            </span>
            <span className="app-tab-label">{d.label}</span>
          </Link>
        );
      })}
    </nav>
  );
}
