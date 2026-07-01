import { ContextHelp } from '@@/PageHeader/ContextHelp';

import { useHeaderContext } from './HeaderContainer';
import { NotificationsMenu } from './NotificationsMenu';
import { UserMenu } from './UserMenu';

export function HeaderTitle() {
  useHeaderContext();

  return (
    <div className="flex items-center">
      <NotificationsMenu />
      <ContextHelp />
      {!window.ddExtension && <UserMenu />}
    </div>
  );
}
