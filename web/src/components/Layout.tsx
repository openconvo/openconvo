import { NavLink, Outlet } from "react-router-dom";

const navigation = [
  { to: "/", label: "Dashboard", end: true },
  { to: "/channels", label: "Archive" },
  { to: "/search", label: "Search" },
  { to: "/bookmarks", label: "Bookmarks" },
  { to: "/discord", label: "Discord" },
  { to: "/backups", label: "Backups" },
  { to: "/settings", label: "Settings" },
];

export default function Layout({
  onLogout,
  logoutError,
}: {
  onLogout: () => void;
  logoutError?: string;
}) {
  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="brand">
          <img src="/favicon.svg" alt="" className="brand-mark" />
          <div>
            <div className="brand-name">OpenConvo</div>
            <div className="brand-tagline">Community archive</div>
          </div>
        </div>
        <nav className="nav">
          {navigation.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => (isActive ? "nav-link active" : "nav-link")}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="sidebar-footer">
          {logoutError && <div className="form-error" role="alert">{logoutError}</div>}
          <button type="button" className="logout-button" onClick={onLogout}>
            Sign out
          </button>
          <a
            href="https://github.com/openconvo/openconvo"
            target="_blank"
            rel="noreferrer"
            className="footer-link"
          >
            Open source · AGPL-3.0
          </a>
        </div>
      </aside>
      <main className="content">
        <Outlet />
      </main>
    </div>
  );
}
