import { Button, Icon, OrgMark, Text } from '../ds';
import { useNavigate } from 'react-router';
import { useOrg } from '../state/OrgProvider';
import type { FrameChrome } from './chrome';
import { ORG_LOGO_SRC, orgName } from './orgIdentity';

export interface PhoneHeaderProps {
  title: string;
  back: FrameChrome['back'];
}

/**
 * Below 960: the org mark and the page title, or a back link in place of the mark on detail pages. The mark is
 * not a control: the server has one org per deployment. The stylesheet hides the header from 960.
 */
export function PhoneHeader({ title, back }: PhoneHeaderProps) {
  const { me } = useOrg();
  const navigate = useNavigate();
  const name = orgName(me);

  return (
    <header className="app-phone-header">
      {back ? (
        <Button variant="ghost" iconOnly icon={<Icon name="chevron-left" />} aria-label={'Back to ' + back.label} onClick={() => void navigate(back.to)} />
      ) : (
        <OrgMark name={name} size={28} {...(me?.org.has_logo ? { src: ORG_LOGO_SRC } : {})} />
      )}
      <Text as="h1" variant="heading" className="app-phone-title">
        {title}
      </Text>
    </header>
  );
}
