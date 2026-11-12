import { useEffect, useState } from "react";
import { fetchSystemStatus, formatBytes, type SystemStatus } from "../api";

const numberFormat = new Intl.NumberFormat();

export default function AttachmentStoragePanel() {
  const [status, setStatus] = useState<SystemStatus | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    const load = () => {
      fetchSystemStatus()
        .then((value) => {
          if (cancelled) return;
          setStatus(value);
          setError("");
        })
        .catch((e: unknown) => {
          if (!cancelled) setError(e instanceof Error ? e.message : String(e));
        });
    };
    load();
    const timer = setInterval(load, 15000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, []);

  const attachments = status?.attachments;
  const total = attachments ? attachments.stored + attachments.pending + attachments.failed : 0;

  return (
    <section className="card">
      <div className="card-heading-row">
        <div>
          <h2>Attachment storage</h2>
          <p className="muted">
            Files posted in archived channels, copied from Discord into OpenConvo's own storage.
          </p>
        </div>
        {attachments && (
          <span
            className={attachments.enabled ? "pill pill-ok" : "pill pill-neutral"}
          >
            {!attachments.enabled ? "Off" : attachments.pending > 0 ? "Downloading" : "Up to date"}
          </span>
        )}
      </div>

      {error && <div className="form-error" role="alert">{error}</div>}

      {attachments && total === 0 && (
        <p className="muted">No attachments in the archived channels yet.</p>
      )}

      {attachments && total > 0 && (
        <>
          <p>
            {numberFormat.format(attachments.stored)} of {numberFormat.format(total)} files stored
            ({formatBytes(attachments.stored_bytes)})
            {attachments.pending > 0 && ` · ${numberFormat.format(attachments.pending)} pending`}
            {attachments.failed > 0 && ` · ${numberFormat.format(attachments.failed)} failed`}
          </p>
          <progress className="attachment-progress" value={attachments.stored} max={total} />
        </>
      )}

      {attachments?.enabled && status?.storage.location && (
        <p className="muted">
          Files are kept in {status.storage.location}. This is the archive's copy of them, not a
          second one: database backups do not include attachment files, a portable export does.
        </p>
      )}

      {attachments && !attachments.enabled && (
        <p className="muted">
          Attachments are archived by reference only. Set{" "}
          <code>OPENCONVO_ATTACHMENTS_ENABLED=true</code> and restart to store the files in
          OpenConvo.
        </p>
      )}
    </section>
  );
}
