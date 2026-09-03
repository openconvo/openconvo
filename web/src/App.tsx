import { useEffect, useState } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import { fetchAuthSession, logout } from "./api";
import Layout from "./components/Layout";
import Dashboard from "./pages/Dashboard";
import ChannelBrowser from "./pages/ChannelBrowser";
import ChannelTimeline from "./pages/ChannelTimeline";
import Login from "./pages/Login";
import MessageContext from "./pages/MessageContext";
import Search from "./pages/Search";
import Bookmarks from "./pages/Bookmarks";
import Backups from "./pages/Backups";
import Discord from "./pages/Discord";
import Settings from "./pages/Settings";

export default function App() {
  const [auth, setAuth] = useState<"loading" | "authenticated" | "anonymous">("loading");
  const [logoutError, setLogoutError] = useState("");

  useEffect(() => {
    let cancelled = false;
    fetchAuthSession()
      .then((session) => {
        if (!cancelled) setAuth(session.authenticated ? "authenticated" : "anonymous");
      })
      .catch(() => {
        if (!cancelled) setAuth("anonymous");
      });
    const requireAuthentication = () => setAuth("anonymous");
    window.addEventListener("openconvo:authentication-required", requireAuthentication);
    return () => {
      cancelled = true;
      window.removeEventListener("openconvo:authentication-required", requireAuthentication);
    };
  }, []);

  if (auth === "loading") {
    return <main className="auth-shell muted">Loading OpenConvo…</main>;
  }
  if (auth === "anonymous") {
    return <Login onAuthenticated={() => setAuth("authenticated")} />;
  }

  // Signing out revokes the session on the server. If that request fails the
  // session is still live, so the operator is told rather than shown a
  // sign-in screen that would suggest otherwise.
  const signOut = () => {
    setLogoutError("");
    logout()
      .then(() => setAuth("anonymous"))
      .catch((error: unknown) => {
        setLogoutError(error instanceof Error ? error.message : String(error));
      });
  };

  return (
    <Routes>
      <Route element={<Layout onLogout={signOut} logoutError={logoutError} />}>
        <Route index element={<Dashboard />} />
        <Route path="channels" element={<ChannelBrowser />} />
        <Route path="channels/:channelId" element={<ChannelTimeline />} />
        <Route path="messages/:messageId" element={<MessageContext />} />
        <Route path="search" element={<Search />} />
        <Route path="bookmarks" element={<Bookmarks />} />
        <Route path="discord" element={<Discord />} />
        <Route path="backups" element={<Backups />} />
        <Route path="settings" element={<Settings />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
