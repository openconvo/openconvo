import { useEffect, useState, type FormEvent, type MouseEvent } from "react";
import {
  backupContentURL,
  createDatabaseBackup,
  downloadDatabaseBackup,
  fetchBackupSettings,
  fetchDatabaseBackups,
  formatBytes,
  saveBackupSettings,
  type BackupProvider,
  type BackupSettings,
  type DatabaseBackup,
} from "../api";

const listInterval = 5000;

function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause);
}

export default function BackupsPanel() {
  const [settings, setSettings] = useState<BackupSettings | null>(null);
  const [backups, setBackups] = useState<DatabaseBackup[]>([]);
  const [saving, setSaving] = useState(false);
  const [starting, setStarting] = useState(false);
  const [message, setMessage] = useState("");
  // Errors the operator caused are kept apart from listing errors, which the
  // next successful poll clears: one failed poll must not leave a banner
  // standing for as long as the page is open.
  const [error, setError] = useState("");
  const [listError, setListError] = useState("");
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let cancelled = false;
    fetchBackupSettings()
      .then((value) => {
        if (!cancelled) setSettings(value);
      })
      .catch((cause: unknown) => {
        if (!cancelled) setError(errorMessage(cause));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // A running backup changes status with no action from the operator, so the
  // listing is polled — while the tab is visible and this panel is mounted,
  // because a hidden tab has nobody to show it to.
  useEffect(() => {
    let cancelled = false;
    const refresh = () => {
      fetchDatabaseBackups()
        .then((rows) => {
          if (cancelled) return;
          setBackups(rows);
          setListError("");
        })
        .catch((cause: unknown) => {
          if (!cancelled) setListError(errorMessage(cause));
        });
    };
    refresh();
    const timer = setInterval(() => {
      if (!document.hidden) refresh();
    }, listInterval);
    const onVisibilityChange = () => {
      if (!document.hidden) refresh();
    };
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      cancelled = true;
      clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [reload]);

  const update = <K extends keyof BackupSettings>(key: K, value: BackupSettings[K]) => {
    setSettings((current) => (current ? { ...current, [key]: value } : current));
  };

  const changeProvider = (provider: BackupProvider) => {
    setSettings((current) => {
      if (!current) return current;
      if (provider === "s3") {
        return { ...current, provider, endpoint: "", region: "us-east-1", force_path_style: false };
      }
      if (provider === "r2") {
        return { ...current, provider, region: "auto", force_path_style: false };
      }
      return { ...current, provider, force_path_style: provider === "custom" && current.force_path_style };
    });
  };

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!settings) return;
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const saved = await saveBackupSettings(settings);
      setSettings(saved);
      setMessage(saved.enabled ? "Backup destination verified and schedule saved." : "Backup settings saved.");
    } catch (cause: unknown) {
      setError(errorMessage(cause));
    } finally {
      setSaving(false);
    }
  };

  const runNow = async () => {
    setStarting(true);
    setError("");
    setMessage("");
    try {
      // A backup already in flight is returned instead of a second one, so
      // the server's created flag decides what actually happened.
      const { created } = await createDatabaseBackup();
      setMessage(created ? "Database backup queued." : "A database backup is already running.");
      setReload((count) => count + 1);
    } catch (cause: unknown) {
      setError(errorMessage(cause));
    } finally {
      setStarting(false);
    }
  };

  const download = (event: MouseEvent<HTMLAnchorElement>, backupId: string) => {
    event.preventDefault();
    setError("");
    downloadDatabaseBackup(backupId).catch((cause: unknown) => setError(errorMessage(cause)));
  };

  return (
    <section className="card backup-card">
      <div className="card-heading-row">
        <div>
          <h2>Database backups</h2>
          <p className="muted">Custom-format PostgreSQL dumps stored outside this server.</p>
        </div>
        <button
          type="button"
          className="primary-button"
          onClick={runNow}
          disabled={starting || !settings?.credentials_configured || !settings?.bucket}
        >
          {starting ? "Queuing…" : "Back up now"}
        </button>
      </div>

      {error && <div className="form-error" role="alert">{error}</div>}
      {message && <div className="form-success">{message}</div>}

      {settings && (
        <form className="backup-form" onSubmit={save}>
          <label className="checkbox-field backup-enable">
            <input
              type="checkbox"
              checked={settings.enabled}
              onChange={(event) => update("enabled", event.target.checked)}
            />
            Run backups automatically
          </label>

          <div className="form-grid">
            <label>
              Provider
              <select
                value={settings.provider}
                onChange={(event) => changeProvider(event.target.value as BackupProvider)}
              >
                <option value="r2">Cloudflare R2</option>
                <option value="s3">Amazon S3</option>
                <option value="backblaze">Backblaze B2</option>
                <option value="custom">Other S3-compatible</option>
              </select>
            </label>

            {settings.provider !== "s3" && (
              <label className="form-span-2">
                S3 API endpoint
                <input
                  type="url"
                  placeholder={
                    settings.provider === "r2"
                      ? "https://<account-id>.r2.cloudflarestorage.com"
                      : "https://s3.example.com"
                  }
                  value={settings.endpoint}
                  onChange={(event) => update("endpoint", event.target.value)}
                />
              </label>
            )}

            <label>
              Bucket
              <input value={settings.bucket} onChange={(event) => update("bucket", event.target.value)} />
            </label>

            <label>
              Region
              <input
                value={settings.region}
                disabled={settings.provider === "r2"}
                onChange={(event) => update("region", event.target.value)}
              />
            </label>

            <label className="form-span-2">
              Object prefix
              <input value={settings.prefix} onChange={(event) => update("prefix", event.target.value)} />
            </label>

            <label>
              Frequency (hours)
              <input
                type="number"
                min={1}
                max={24 * 31}
                value={settings.interval_hours}
                onChange={(event) => update("interval_hours", Number(event.target.value))}
              />
            </label>

            <label>
              Keep recent backups
              <input
                type="number"
                min={1}
                max={1000}
                value={settings.retention_count}
                onChange={(event) => update("retention_count", Number(event.target.value))}
              />
            </label>
          </div>

          {settings.provider === "custom" && (
            <label className="checkbox-field">
              <input
                type="checkbox"
                checked={settings.force_path_style}
                onChange={(event) => update("force_path_style", event.target.checked)}
              />
              Use path-style bucket URLs (usually required by MinIO)
            </label>
          )}

          <div className="backup-form-footer">
            <span className={settings.credentials_configured ? "credential-ok" : "credential-missing"}>
              {settings.credentials_configured
                ? "Credentials loaded from environment"
                : "Set BACKUP_S3_ACCESS_KEY and BACKUP_S3_SECRET_KEY, then restart"}
            </span>
            <button type="submit" className="secondary-button" disabled={saving}>
              {saving ? "Saving…" : settings.enabled ? "Save and verify" : "Save settings"}
            </button>
          </div>
        </form>
      )}

      <div className="backup-list-heading">
        <h3>Recent backups</h3>
        <span className="muted">Newest first</span>
      </div>
      {listError && <div className="form-error">{listError}</div>}
      {backups.length === 0 ? (
        <p className="muted">No database backups yet.</p>
      ) : (
        <div className="table-scroll">
          <table className="backup-table">
            <thead>
              <tr>
                <th>Created</th>
                <th>Status</th>
                <th>Size</th>
                <th>Destination</th>
                <th><span className="visually-hidden">Download</span></th>
              </tr>
            </thead>
            <tbody>
              {backups.map((backup) => (
                <tr key={backup.id} title={backup.error || undefined}>
                  <td>{new Date(backup.created_at).toLocaleString()}</td>
                  <td><BackupStatus backup={backup} /></td>
                  <td>{backup.size > 0 ? formatBytes(backup.size) : "—"}</td>
                  <td>
                    {backup.provider} · {backup.bucket}
                    {backup.error && <div className="backup-error">{backup.error}</div>}
                  </td>
                  <td>
                    {backup.download_available && (
                      <a
                        href={backupContentURL(backup.id)}
                        onClick={(event) => download(event, backup.id)}
                      >
                        Download
                      </a>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function BackupStatus({ backup }: { backup: DatabaseBackup }) {
  const className =
    backup.status === "succeeded"
      ? "pill pill-ok"
      : backup.status === "failed"
        ? "pill pill-bad"
        : "pill pill-neutral";
  return <span className={className}>{backup.status}</span>;
}
