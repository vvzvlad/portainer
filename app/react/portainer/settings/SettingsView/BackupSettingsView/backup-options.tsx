import { DownloadCloud } from 'lucide-react';

import { BadgeIcon } from '@@/BadgeIcon';

export enum BackupFormType {
  S3 = 's3',
  File = 'file',
}

export const options = [
  {
    id: 'backup_file',
    icon: <BadgeIcon icon={DownloadCloud} />,
    label: 'Download backup file',
    value: BackupFormType.File,
  },
];
