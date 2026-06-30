import { Database } from 'lucide-react';

import { StorageClass } from '@/react/portainer/environments/types';

import { Icon } from '@@/Icon';
import { FormSectionTitle } from '@@/form-components/FormSectionTitle';

type Props = {
  storageClass: StorageClass;
};

export function StorageQuotaItem({ storageClass }: Props) {
  return (
    <div key={storageClass.Name}>
      <FormSectionTitle>
        <div className="vertical-center text-muted inline-flex gap-1 align-top">
          <Icon icon={Database} className="!mt-0.5 flex-none" />
          <span>{storageClass.Name}</span>
        </div>
      </FormSectionTitle>
      <hr className="mb-0 mt-2 w-full" />
    </div>
  );
}
