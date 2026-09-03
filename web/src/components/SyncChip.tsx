import type { SyncRow } from "../api";

export default function SyncChip({ row }: { row: SyncRow }) {
  const count = row.message_count.toLocaleString();
  const label =
    row.status === "importing"
      ? "importing · " + count + " msgs"
      : row.status === "synced"
        ? "synced · " + count + " msgs"
        : row.status;
  const className =
    row.status === "synced"
      ? "pill pill-ok"
      : row.status === "error"
        ? "pill pill-bad"
        : "pill pill-neutral";

  return (
    <span className={className} title={row.last_error || undefined}>
      {label}
    </span>
  );
}
