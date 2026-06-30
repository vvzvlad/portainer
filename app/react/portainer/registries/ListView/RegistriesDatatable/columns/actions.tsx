import { CellContext } from '@tanstack/react-table';

import { DecoratedRegistry } from '../types';

import { columnHelper } from './helper';
import { DefaultRegistryAction } from './DefaultRegistryAction';

export const actions = columnHelper.display({
  header: 'Actions',
  cell: Cell,
});

function Cell({
  row: { original: item },
}: CellContext<DecoratedRegistry, unknown>) {
  if (!item.Id) {
    return <DefaultRegistryAction />;
  }

  return null;
}
