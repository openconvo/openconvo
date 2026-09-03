import { useEffect, useState, type FormEvent } from "react";
import {
  fetchEmbeddingSettings,
  saveEmbeddingSettings,
  type EmbeddingSettings,
} from "../api";

export default function EmbeddingsPanel() {
  const [settings, setSettings] = useState<EmbeddingSettings | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");

  useEffect(() => {
    let cancelled = false;
    fetchEmbeddingSettings()
      .then((value) => {
        if (!cancelled) setSettings(value);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!settings?.enabled || settings.generation_status === "active") return;
    let cancelled = false;
    const timer = setInterval(() => {
      fetchEmbeddingSettings()
        .then((value) => {
          if (cancelled) return;
          // A poll refreshes only what the server owns. Replacing the whole
          // record would quietly undo an opt-in the operator has ticked but
          // not yet saved.
          setSettings((current) =>
            current
              ? {
                  ...current,
                  credentials_configured: value.credentials_configured,
                  source: value.source,
                  generation_status: value.generation_status,
                  embedded_messages: value.embedded_messages,
                  eligible_messages: value.eligible_messages,
                }
              : value,
          );
        })
        .catch((e: unknown) => {
          if (!cancelled) setError(e instanceof Error ? e.message : String(e));
        });
    }, 5000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [settings?.enabled, settings?.generation_status]);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!settings) return;
    setSaving(true);
    setError("");
    setMessage("");
    try {
      const saved = await saveEmbeddingSettings(settings);
      setSettings(saved);
      setMessage(saved.enabled ? "Embedding provider verified; message indexing has started." : "Message embeddings disabled.");
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="card">
      <h2>Message embeddings</h2>
      <p className="muted">
        Optional, derived semantic index. Enabling sends every non-empty, non-deleted archived
        message to OpenAI. Canonical messages and full-text search do not depend on it.
      </p>

      {error && <div className="form-error">{error}</div>}
      {message && <div className="form-success">{message}</div>}

      {settings && (
        <form onSubmit={save}>
          <label className="checkbox-field">
            <input
              type="checkbox"
              checked={settings.enabled}
              onChange={(event) => setSettings({ ...settings, enabled: event.target.checked })}
            />
            Send archived message text to OpenAI and build embeddings
          </label>

          <dl className="settings-summary">
            <div><dt>Provider</dt><dd>OpenAI</dd></div>
            <div><dt>Model</dt><dd><code>{settings.model}</code></dd></div>
            <div><dt>Dimensions</dt><dd>{settings.dimensions}</dd></div>
            <div>
              <dt>Progress</dt>
              <dd>
                {settings.embedded_messages.toLocaleString()} / {settings.eligible_messages.toLocaleString()}
                {settings.generation_status ? ` · ${settings.generation_status}` : ""}
              </dd>
            </div>
          </dl>

          <div className="backup-form-footer">
            <span className={settings.credentials_configured ? "credential-ok" : "credential-missing"}>
              {settings.credentials_configured
                ? "OPENAI_API_KEY loaded from environment"
                : "Set OPENAI_API_KEY, then restart OpenConvo"}
            </span>
            <button
              type="submit"
              className="secondary-button"
              disabled={saving || (settings.enabled && !settings.credentials_configured)}
            >
              {saving ? "Saving…" : "Save embedding settings"}
            </button>
          </div>
        </form>
      )}
    </div>
  );
}
