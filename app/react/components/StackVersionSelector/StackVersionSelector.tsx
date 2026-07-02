import { isoDateFromTimestamp } from '@/portainer/filters/filters';
import { StackFileVersionInfo } from '@/react/common/stacks/types';

interface Props {
  versions?: number[];
  /**
   * Optional richer metadata (date/author/note) used to build the option
   * labels. Looked up by version number; falls back to the bare version when a
   * given version has no metadata.
   */
  versionsInfo?: StackFileVersionInfo[];
  onChange(value: number): void;
}

/**
 * Build a human-readable label for a version, e.g. `v3 · 2026-07-02 14:12 · admin`.
 * Falls back to just the version number when no metadata is available.
 */
function buildVersionLabel(version: number, info?: StackFileVersionInfo) {
  const parts = [`v${version}`];
  if (info?.CreatedAt) {
    parts.push(isoDateFromTimestamp(info.CreatedAt));
  }
  if (info?.CreatedBy) {
    parts.push(info.CreatedBy);
  }
  return parts.join(' · ');
}

export function StackVersionSelector({
  versions,
  versionsInfo,
  onChange,
}: Props) {
  if (!versions || versions.length === 0) {
    return null;
  }

  const showSelector = versions.length > 1;

  const versionOptions = versions.map((version) => ({
    value: version,
    label: buildVersionLabel(
      version,
      versionsInfo?.find((info) => info.Version === version)
    ),
  }));

  return (
    <div className="flex">
      {!showSelector && (
        <>
          <label className="text-muted mr-2" htmlFor="version_id">
            <span>Version:</span>
          </label>
          <span className="text-muted" id="version_id">
            {versionOptions[0].label}
          </span>
        </>
      )}

      {showSelector && (
        <div className="text-muted">
          <label className="mr-2" htmlFor="version_id">
            <span>Version:</span>
          </label>
          <select
            className="form-select"
            data-cy="version-selector"
            id="version_id"
            style={{
              height: '24px',
              borderRadius: '4px',
              borderColor: 'hsl(0, 0%, 80%)',
              padding: '2px 8px',
            }}
            onChange={(e) => onChange(parseInt(e.target.value, 10))}
          >
            {versionOptions.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </div>
      )}
    </div>
  );
}
