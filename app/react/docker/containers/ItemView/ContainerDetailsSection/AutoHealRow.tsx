import { DetailsTable } from '@@/DetailsTable';

interface Props {
  labels?: Record<string, string>;
}

// Label keys (with community aliases) mirroring the backend
// (api/containerautomation/labels.go).
const ENABLE_LABEL = 'io.portainer.autoheal.enable';
const ENABLE_LABEL_ALIAS = 'autoheal';
const STOP_TIMEOUT_LABEL = 'io.portainer.autoheal.stop-timeout';
const STOP_TIMEOUT_LABEL_ALIAS = 'autoheal.stop.timeout';
const RETRIES_LABEL = 'io.portainer.autoheal.retries';

function parseBool(value?: string) {
  if (value === undefined) {
    return undefined;
  }
  // Mirror Go's strconv.ParseBool (used by the backend label parser): accept
  // 1/t/true case-insensitively as truthy. Any other present-but-invalid value
  // (including 0/f/false and garbage) counts as present & false, matching how
  // the backend treats an unparseable enable label.
  const normalized = value.toLowerCase();
  return normalized === '1' || normalized === 't' || normalized === 'true';
}

/**
 * AutoHealRow shows the per-container auto-heal opt-in state, resolved from the
 * container's immutable Docker labels. It is read-only: because labels cannot be
 * changed on a running container, opt-in is set through the Create/Edit form
 * labels and the global behavior is controlled in Settings.
 */
export function AutoHealRow({ labels }: Props) {
  const enabled =
    parseBool(labels?.[ENABLE_LABEL]) ?? parseBool(labels?.[ENABLE_LABEL_ALIAS]);

  let stateLabel: string;
  if (enabled === true) {
    stateLabel = 'Enabled';
  } else if (enabled === false) {
    stateLabel = 'Disabled (opted out)';
  } else {
    stateLabel = 'Not labeled (follows global scope)';
  }

  const stopTimeout =
    labels?.[STOP_TIMEOUT_LABEL] ?? labels?.[STOP_TIMEOUT_LABEL_ALIAS];
  const retries = labels?.[RETRIES_LABEL];

  return (
    <DetailsTable.Row label="Auto-heal">
      <div className="space-y-1">
        <div>{stateLabel}</div>
        {stopTimeout && (
          <div className="small text-muted">
            Stop timeout: {stopTimeout}s
          </div>
        )}
        {retries && (
          <div className="small text-muted">Max retries: {retries}</div>
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
