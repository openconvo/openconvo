import type { SyncRow } from "../api";

export default function SyncChip({
  row,
  showErrorDetails = false,
}: {
  row: SyncRow;
  showErrorDetails?: boolean;
}) {
  const count = row.message_count.toLocaleString();
  const missingAccess =
    row.status === "error" && /HTTP 403, code 50001: Missing Access/i.test(row.last_error ?? "");
  const label =
    row.status === "importing"
      ? "importing · " + count + " msgs"
      : row.status === "synced"
        ? "synced · " + count + " msgs"
        : missingAccess
          ? "Missing access"
          : row.status;
  const className =
    row.status === "synced"
      ? "pill pill-ok"
      : row.status === "error"
        ? "pill pill-bad"
        : "pill pill-neutral";

  return (
    <>
      <span className={className} title={showErrorDetails ? undefined : row.last_error || undefined}>
        {label}
      </span>
      {showErrorDetails && row.status === "error" && (
        <p className="sync-error-detail">
          {missingAccess ? (
            <>
              Discord returned 403 / 50001 “Missing Access”. Check the bot's View Channel and
              Read Message History permissions, including category overrides. Once access is
              restored, OpenConvo automatically retries
              {row.backfill_complete ? " syncing" : " the backfill"}.
            </>
          ) : row.last_error || "Sync failed. Check the OpenConvo server logs."}
        </p>
      )}
    </>
  );
}
