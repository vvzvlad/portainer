import { useMemo } from 'react';

import { inheritedName } from './inheritedName';
import { name } from './name';
import { type } from './type';

export function useColumns({ inheritFrom }: { inheritFrom: boolean }) {
  return useMemo(
    () => [inheritFrom ? inheritedName : name, type],
    [inheritFrom]
  );
}
