import { AutoUpdateScope } from '../../types';

export interface Values {
  enabled: boolean;
  pollInterval: string;
  scope: AutoUpdateScope;
  cleanup: boolean;
}
