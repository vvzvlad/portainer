import { useMemo } from 'react';

export function useBaseApiQueryParams(searchTerm: string) {
  return useMemo(
    () => ({
      provisioned: true,
      updateInformation: false,
      k8sEnvAdmin: true,
      search: searchTerm || undefined,
    }),
    [searchTerm]
  );
}
