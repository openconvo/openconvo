import { useState, type FormEvent } from "react";
import { login } from "../api";
import { errorMessage } from "./errorMessage";

export default function Login({ onAuthenticated }: { onAuthenticated: () => void }) {
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      await login(password);
      setPassword("");
      onAuthenticated();
    } catch (err) {
      setError(errorMessage(err, "Sign in failed. Try again."));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <main className="auth-shell">
      <section className="auth-card" aria-labelledby="login-title">
        <div className="auth-brand">
          <img src="/favicon.svg" alt="" className="brand-mark" />
          <div>
            <div className="brand-name">OpenConvo</div>
            <div className="brand-tagline">Community archive</div>
          </div>
        </div>
        <h1 id="login-title">Administrator sign in</h1>
        <p className="muted">This archive is private.</p>
        <form onSubmit={submit} className="auth-form">
          <label htmlFor="admin-password">Password</label>
          <input
            id="admin-password"
            type="password"
            autoComplete="current-password"
            autoFocus
            required
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
          {error && <div className="form-error" role="alert">{error}</div>}
          <button type="submit" disabled={submitting}>
            {submitting ? "Signing in…" : "Sign in"}
          </button>
        </form>
      </section>
    </main>
  );
}
