import React from 'react';
import { Icon } from '../Icon/Icon.jsx';
import { OrgMark } from '../OrgMark/OrgMark.jsx';
import { usePopover, MenuItem } from '../Menu/Menu.jsx';

export function OrgSwitcher({ orgs = [], current, onSelect, onCreate, defaultOpen }) {
  const cur = orgs.find((o) => o.id === current) || orgs[0] || {};
  const p = usePopover(defaultOpen);
  const close = () => p.setOpen(false);
  return (
    <div ref={p.ref} style={{ position: 'relative' }}>
      <button className="kv-orgswitch" onClick={() => p.setOpen(!p.open)} aria-expanded={p.open} aria-haspopup="menu">
        <OrgMark name={cur.name} src={cur.src} color={cur.color} size={28} />
        <span className="kv-orgswitch-name">{cur.name}</span>
        <Icon name="chevron-down" />
      </button>
      {p.open && (
        <div role="menu" className="kv-menu" style={{ position: 'absolute', top: 'calc(100% + 6px)', left: 0 }}>
          <div className="kv-menu-label">Your orgs</div>
          {orgs.map((o) => (
            <MenuItem key={o.id} close={close} onSelect={() => onSelect && onSelect(o.id)}>
              <OrgMark name={o.name} src={o.src} color={o.color} size={20} /><span>{o.name}</span>
              {o.id === cur.id && <span className="kv-menu-right"><Icon name="check" /></span>}
            </MenuItem>
          ))}
          <div className="kv-menu-sep" role="separator" />
          <MenuItem close={close} icon="plus" label="New org" onSelect={onCreate} />
        </div>
      )}
    </div>
  );
}
