import { UserX } from 'lucide-react';
import { useState } from 'react';

import { Datatable } from '@@/datatables';
import { createPersistedStore } from '@@/datatables/types';
import { useTableState } from '@@/datatables/useTableState';
import { withMeta } from '@@/datatables/extend-options/withMeta';
import { mergeOptions } from '@@/datatables/extend-options/mergeOptions';

import { useColumns } from './columns/useColumns';
import { Access } from './types';
import { RemoveAccessButton } from './RemoveAccessButton';

type Props = {
  tableKey: string;
  dataset?: Array<Access>;
  onRemove(items: Array<Access>): void;
  inheritFrom?: boolean;
  isLoading: boolean;
};

export function AccessDatatable({
  dataset,
  tableKey,
  onRemove,
  inheritFrom = false,
  isLoading,
}: Props) {
  const columns = useColumns({ inheritFrom });
  const [store] = useState(() => createPersistedStore(tableKey));
  const tableState = useTableState(store, tableKey);

  return (
    <Datatable
      data-cy="access-datatable"
      title="Access"
      titleIcon={UserX}
      dataset={dataset || []}
      isLoading={isLoading}
      columns={columns}
      settingsManager={tableState}
      getRowId={(row) => `${row.Type}-${row.Id}`}
      extendTableOptions={mergeOptions(
        withMeta({
          table: 'access-table',
        })
      )}
      isRowSelectable={({ original: item }) => !inheritFrom || !item.Inherited}
      renderTableActions={(selectedItems) => (
        <RemoveAccessButton items={selectedItems} onClick={onRemove} />
      )}
      description={
        <>
          {inheritFrom && (
            <div className="small text-muted">
              <div>
                Access tagged as <code>inherited</code> are inherited from the
                group access. They cannot be removed or modified at the
                environment level but they can be overridden.
              </div>
              <div>
                Access tagged as <code>override</code> are overriding the group
              </div>
            </div>
          )}
        </>
      }
    />
  );
}
