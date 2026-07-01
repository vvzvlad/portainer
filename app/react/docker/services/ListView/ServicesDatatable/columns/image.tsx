import { CellContext } from '@tanstack/react-table';

import { ServiceViewModel } from '@/docker/models/service';
import { hideShaSum } from '@/docker/filters/utils';

import { columnHelper } from './helper';

export const image = columnHelper.accessor((item) => item.Image, {
  id: 'image',
  header: 'Image',
  cell: Cell,
});

function Cell({ getValue }: CellContext<ServiceViewModel, string>) {
  const value = hideShaSum(getValue());
  return <>{value}</>;
}
