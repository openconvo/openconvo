import { useEffect, useState } from "react";
import { fetchUpdateStatus, type UpdateStatus } from "../api";

type State =
  | { phase: "loading" }
  | { phase: "ready"; status: UpdateStatus }
  | { phase: "error"; message: string };

const upgradeGuide = "https://github.com/openconvo/openconvo/blob/main/docs/upgrades.md";

// The release URL is whatever the release feed returned, and a link is one of
// the few places a javascript: or data: URL would run in this origin. Only
// ever link out over http(s).
function webURL(value: string | undefined): string | undefined {
  if (!value) return undefined;
  try {
    const parsed = new URL(value);
    return parsed.protocol === "https:" || parsed.protocol === "http:" ? parsed.href : undefined;
  } catch {
    return undefined;
  }
}

export default function UpdatePanel() {
  const [state, setState] = useState<State>({ phase: "loading" });
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    let cancelled = false;
    fetchUpdateStatus()
      .then((status) => {
        if (!cancelled) setState({ phase: "ready", status });
      })
      .catch((error: unknown) => {
        if (!cancelled) {
          setState({
            phase: "error",
            message: error instanceof Error ? error.message : String(error),
          });
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const copyCommand = async (command: string) => {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(command);
      } else {
        const field = document.createElement("textarea");
        field.value = command;
        field.setAttribute("readonly", "");
        field.style.position = "fixed";
        field.style.opacity = "0";
        document.body.appendChild(field);
        field.select();
        let copied = false;
        try {
          copied = document.execCommand("copy");
        } finally {
          field.remove();
        }
        if (!copied) throw new Error("copy command was rejected");
      }
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2500);
    } catch {
      setCopied(false);
    }
  };

  if (state.phase === "loading") {
    return <div className="card muted">Checking for OpenConvo updates…</div>;
  }

  if (state.phase === "error") {
    return (
      <div className="card update-card">
        <h2>Updates</h2>
        <p className="muted">
          The latest release could not be checked. OpenConvo sends no archive data during this
          check. <a href={upgradeGuide} target="_blank" rel="noreferrer">Check manually</a>.
        </p>
      </div>
    );
  }

  const status = state.status;
  const releaseURL = webURL(status.release_url);
  if (status.reason === "development-build") {
    return (
      <div className="card update-card">
        <h2>Updates</h2>
        <p className="muted">
          This is a development build, so it cannot be compared with published releases. See the{" "}
          <a href={upgradeGuide} target="_blank" rel="noreferrer">upgrade guide</a>.
        </p>
      </div>
    );
  }

  if (!status.update_available) {
    return (
      <div className="card update-card">
        <div className="card-heading-row">
          <div>
            <h2>Updates</h2>
            <p className="muted">OpenConvo {status.current_version} is the latest release.</p>
          </div>
          <span className="pill pill-ok">Up to date</span>
        </div>
      </div>
    );
  }

  if (!status.command_upgrade_allowed || !status.upgrade_command) {
    return (
      <div className="card notice-card update-card">
        <h2>OpenConvo {status.latest_version} is available</h2>
        <p>
          This release crosses a compatibility boundary and needs the manual upgrade steps. Review{" "}
          {releaseURL && (
            <><a href={releaseURL} target="_blank" rel="noreferrer">the release notes</a> and </>
          )}
          <a href={upgradeGuide} target="_blank" rel="noreferrer">the upgrade guide</a> before updating.
        </p>
      </div>
    );
  }

  return (
    <div className="card notice-card update-card">
      <div className="card-heading-row">
        <div>
          <h2>OpenConvo {status.latest_version} is available</h2>
          <p className="muted">Installed: {status.current_version}</p>
        </div>
        {releaseURL && (
          <a href={releaseURL} target="_blank" rel="noreferrer">Release notes</a>
        )}
      </div>
      <p>Run this from the directory containing <code>compose.yaml</code>:</p>
      <pre className="upgrade-command"><code>{status.upgrade_command}</code></pre>
      <div className="upgrade-actions">
        <button
          type="button"
          className="primary-button"
          onClick={() => void copyCommand(status.upgrade_command ?? "")}
        >
          {copied ? "Copied" : "Copy upgrade command"}
        </button>
        <a href={upgradeGuide} target="_blank" rel="noreferrer">Upgrade guide</a>
      </div>
    </div>
  );
}
