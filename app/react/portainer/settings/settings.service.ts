import axios, { parseAxiosError } from '@/portainer/services/axios/axios';

import { PublicSettingsResponse, DefaultRegistry, Settings } from './types';

export async function getPublicSettings() {
  try {
    const { data } = await axios.get<PublicSettingsResponse>(
      buildUrl('public')
    );
    return data;
  } catch (e) {
    throw parseAxiosError(
      e as Error,
      'Unable to retrieve application settings'
    );
  }
}

export async function getGlobalDeploymentOptions() {
  const publicSettings = await getPublicSettings();
  return publicSettings.GlobalDeploymentOptions;
}

export async function getSettings() {
  try {
    const { data } = await axios.get<Settings>(buildUrl());
    return data;
  } catch (e) {
    throw parseAxiosError(
      e as Error,
      'Unable to retrieve application settings'
    );
  }
}

type OptionalSettings = Omit<Partial<Settings>, 'Edge' | 'ContainerAutomation'> & {
  Edge?: Partial<Settings['Edge']>;
  // ContainerAutomation blocks (AutoHeal / AutoUpdate) are saved independently
  // by their own settings panels, so each may be sent on its own.
  ContainerAutomation?: Partial<Settings['ContainerAutomation']>;
};

export async function updateSettings(settings: OptionalSettings) {
  try {
    const { data } = await axios.put<Settings>(buildUrl(), settings);
    return data;
  } catch (e) {
    throw parseAxiosError(e as Error, 'Unable to update application settings');
  }
}

export async function updateDefaultRegistry(
  defaultRegistry: Partial<DefaultRegistry>
) {
  try {
    await axios.put(buildUrl('default_registry'), defaultRegistry);
  } catch (e) {
    throw parseAxiosError(
      e as Error,
      'Unable to update default registry settings'
    );
  }
}

export function buildUrl(subResource?: string, action?: string) {
  let url = 'settings';
  if (subResource) {
    url += `/${subResource}`;
  }

  if (action) {
    url += `/${action}`;
  }

  return url;
}
