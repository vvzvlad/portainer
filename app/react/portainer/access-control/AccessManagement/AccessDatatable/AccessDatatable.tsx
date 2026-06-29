import { UserX } from 'lucide-react';
import { useMemo, useState } from 'react';

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
  const rolesState = useRolesState();

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
          roles: rolesState,
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

function useRolesState() {
  const [teamRoles, setTeamRoles] = useState<
    Record<number, number | undefined>
  >({});
  const [userRoles, setUserRoles] = useState<
    Record<number, number | undefined>
  >({});

  const count = useMemo(
    () => Object.keys(teamRoles).length + Object.keys(userRoles).length,
    [teamRoles, userRoles]
  );

  return { getRoleValue, setRolesValue, getUpdate, count };

  function getRoleValue(id: number, entity: 'user' | 'team') {
    if (entity === 'team') {
      return teamRoles[id];
    }
    return userRoles[id];
  }

  function setRolesValue(
    id: number,
    entity: 'user' | 'team',
    value: number | undefined
  ) {
    if (entity === 'team') {
      setTeamRoles(updater);

      return;
    }

    setUserRoles(updater);

    function updater(roles: Record<number, number | undefined>) {
      const newRoles = { ...roles };
      if (typeof value === 'undefined') {
        delete newRoles[id];
      } else {
        newRoles[id] = value;
      }
      return newRoles;
    }
  }

  function getUpdate() {
    return {
      users: userRoles,
      teams: teamRoles,
    };
  }
}
