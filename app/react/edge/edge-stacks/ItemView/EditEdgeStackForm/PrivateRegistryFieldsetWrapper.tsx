import {
  PrivateRegistryFieldset,
  REGISTRY_CREDENTIALS_ENABLED,
} from '@/react/edge/edge-stacks/components/PrivateRegistryFieldset';
import { useRegistries } from '@/react/portainer/registries/queries/useRegistries';

import { FormValues } from './types';

export function PrivateRegistryFieldsetWrapper({
  value,
  error,
  onChange,
  values,
  isGit,
}: {
  value: FormValues['privateRegistryId'];
  error?: string;
  onChange: (value?: number) => void;
  values: {
    fileContent?: string;
    file?: File;
  };
  isGit?: boolean;
}) {
  const registriesQuery = useRegistries({ hideDefault: true });

  if (!registriesQuery.data) {
    return null;
  }

  return (
    <PrivateRegistryFieldset
      value={value}
      formInvalid={!values.file && !values.fileContent && !isGit}
      errorMessage={error}
      registries={registriesQuery.data}
      onReload={() => matchRegistry()}
      onChange={(value) => {
        onChange(value);
        if (value === REGISTRY_CREDENTIALS_ENABLED) {
          // Enabled, need to match registry
          matchRegistry();
        }
      }}
      method={isGit ? 'repository' : 'file'}
    />
  );

  function matchRegistry() {
    if (isGit) {
      return;
    }

    // Registry auto-matching is a BE-only feature and is a no-op in CE.
  }
}
