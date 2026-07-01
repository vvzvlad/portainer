import { DetailsTable } from '@@/DetailsTable';

interface Props {
  labels?: Record<string, string>;
}

// Label keys (with watchtower aliases) mirroring the backend
// (api/containerautomation/labels.go).
const ENABLE_LABEL = 'io.portainer.update.enable';
const ENABLE_LABEL_ALIAS = 'com.centurylinklabs.watchtower.enable';
const MONITOR_ONLY_LABEL = 'io.portainer.update.monitor-only';
const MONITOR_ONLY_LABEL_ALIAS = 'com.centurylinklabs.watchtower.monitor-only';

function parseBool(value?: string) {
  if (value === undefined) {
    return undefined;
  }
  // Mirror Go's strconv.ParseBool (used by the backend label parser): accept
  // 1/t/true case-insensitively as truthy. Any other present-but-invalid value
  // (including 0/f/false and garbage) counts as present & false, matching how
  // the backend treats an unparseable label.
  const normalized = value.toLowerCase();
  return normalized === '1' || normalized === 't' || normalized === 'true';
}

/**
 * AutoUpdateRow shows the per-container auto-update opt-in state, resolved from
 * the container's immutable Docker labels. It is read-only: because labels
 * cannot be changed on a running container, opt-in is set through the
 * Create/Edit form labels and the global behavior is controlled in Settings.
 */
export function AutoUpdateRow({ labels }: Props) {
  const enabled =
    parseBool(labels?.[ENABLE_LABEL]) ?? parseBool(labels?.[ENABLE_LABEL_ALIAS]);
  const monitorOnly =
    parseBool(labels?.[MONITOR_ONLY_LABEL]) ??
    parseBool(labels?.[MONITOR_ONLY_LABEL_ALIAS]);

  let stateLabel: string;
  if (enabled === true) {
    stateLabel = 'Enabled';
  } else if (enabled === false) {
    stateLabel = 'Disabled (opted out)';
  } else {
    stateLabel = 'Not labeled (follows global scope)';
  }

  return (
    <DetailsTable.Row label="Auto-update">
      <div className="space-y-1">
        <div>{stateLabel}</div>
        {monitorOnly === true && (
          <div className="small text-muted">
            Monitor-only: updates are detected but never applied automatically.
          </div>
        )}
        <div className="small text-muted">
          Set via the <code>{ENABLE_LABEL}</code> label (immutable at runtime;
          edit through the container Create/Edit form). Global behavior is
          configured in Settings.
        </div>
      </div>
    </DetailsTable.Row>
  );
}
