import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Link, NavLink, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { api, setUnauthorizedHandler } from './api.js';
import Login from './Login.jsx';
import Sites from './pages/Sites.jsx';
import Databases from './pages/Databases.jsx';
import WordPress from './pages/WordPress.jsx';
import Malware from './pages/Malware.jsx';
import Cron from './pages/Cron.jsx';
import Import from './pages/Import.jsx';
import Files from './pages/Files.jsx';
import Backups from './pages/Backups.jsx';
import Ssl from './pages/Ssl.jsx';
import Logs from './pages/Logs.jsx';
import Firewall from './pages/Firewall.jsx';
import Waf from './pages/Waf.jsx';
import Users from './pages/Users.jsx';
import Packages from './pages/Packages.jsx';
import Php from './pages/Php.jsx';
import System from './pages/System.jsx';
import Account from './pages/Account.jsx';
import Settings from './pages/Settings.jsx';
import Dashboard from './pages/Dashboard.jsx';
import StatusRail from './StatusRail.jsx';
import { FEATURES } from './features.js';


// The sidebar is the four headings and nothing else. Everything under them
// is one click away on the dashboard, which is what it is for; a sidebar
// long enough to need reading is one people stop reading.
const MENU = [
  { to: '/', label: 'Dashboard', end: true },
  { to: '/sites', label: 'Main' },
  { to: '/users', label: 'Server', roles: ['admin', 'reseller'] },
  { to: '/account', label: 'Account' },
];

function visible(entry, role) {
  return !entry.roles || entry.roles.includes(role);
}

export default function App() {
  const [me, setMe] = useState(null);
  const [brand, setBrand] = useState({ name: 'OPanel' });
  const [loading, setLoading] = useState(true);
  const [quota, setQuota] = useState(null);
  // The server's own address, shown in the top bar: on a panel reached by a
  // name, the address is the thing somebody needs when they go to point a
  // domain at it.
  const [host, setHost] = useState({});
  const accountMenu = useRef(null);
  const closeAccountMenu = () => {
    if (accountMenu.current) accountMenu.current.open = false;
  };
  const location = useLocation();
  const navigate = useNavigate();

  const signOut = useCallback(() => setMe(null), []);

  useEffect(() => {
    setUnauthorizedHandler(signOut);
  }, [signOut]);

  useEffect(() => {
    api.get('/branding').then(setBrand).catch(() => {});
  }, []);

  useEffect(() => {
    // The favicon and title follow the brand, so a reseller's customers see
    // the reseller's name rather than ours.
    document.title = brand.name || 'Control panel';
    if (brand.favicon) {
      let link = document.querySelector('link[rel="icon"]');
      if (!link) {
        link = document.createElement('link');
        link.rel = 'icon';
        document.head.appendChild(link);
      }
      link.href = brand.favicon;
    }
  }, [brand]);

  useEffect(() => {
    api.get('/auth/me')
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (!me) return;
    // A quota that is configured but not enforced is the kind of thing an
    // operator must be told about every time, not once.
    api.get('/quota/status').then(setQuota).catch(() => setQuota(null));
    api.get('/system/address').then(setHost).catch(() => setHost({}));
  }, [me]);

  // A details element stays open when the page changes under it.
  useEffect(closeAccountMenu, [location.pathname]);

  if (loading) return null;
  if (!me) return <Login brand={brand} onSignedIn={setMe} />;

  const role = me.role;
  const title = pageTitle(location.pathname);

  return (
    <div className="shell">
      <nav className="sidebar">
        <div className="brand">
          {brand.logo ? <img src={brand.logo} alt={brand.name} /> : brand.name}
        </div>

        {MENU.filter((item) => visible(item, role)).map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            className={({ isActive }) => (isActive ? 'active' : undefined)}
          >
            {item.label}
          </NavLink>
        ))}

        <div className="spacer" />
      </nav>

      <div className="main">
        {me.impersonated && (
          <div className="banner bad" style={{ display: 'flex', gap: '.8rem', alignItems: 'center' }}>
            <span style={{ flex: 1 }}>
              You are signed in as <strong>{me.username}</strong>
              {me.impersonated_by ? <> — your own account is <strong>{me.impersonated_by}</strong></> : null}.
              Everything you do here is done as this customer.
            </span>
            <button
              type="button"
              onClick={async () => {
                try {
                  const res = await api.post('/auth/impersonate/stop', {});
                  setMe(res.user);
                  navigate('/users');
                } catch {
                  signOut();
                }
              }}
            >
              Back to my account
            </button>
          </div>
        )}

        {quota && !quota.enforced && role === 'admin' && (
          <div className="banner">
            {quota.pending_reboot
              ? 'Disk quotas are configured but not active yet — reboot the server to start enforcing them.'
              : 'Disk quotas are not enforced on this server. Package disk limits are advisory only.'}
          </div>
        )}

        <header className="topbar">
          <h1>{title}</h1>
          <div className="spacer" />
          {host.ipv4 && (
            <span className="addr" title="This server's address">{host.ipv4}</span>
          )}
          <details className="menu" ref={accountMenu}>
            <summary>{me.username} ▾</summary>
            <div className="sheet">
              <div className="who">
                Signed in as <strong>{me.username}</strong><br />{role}
              </div>
              <Link to="/account" onClick={closeAccountMenu}>Account settings</Link>
              <button
                type="button"
                onClick={async () => {
                  closeAccountMenu();
                  try { await api.post('/auth/logout'); } catch { /* signing out anyway */ }
                  signOut();
                }}
              >
                Sign out
              </button>
            </div>
          </details>
        </header>

        <div className="body">
          <main className="content">
            <Routes>
              {/* The dashboard is a way in, not a report: it lists what the
                  panel can do, grouped the way the sidebar is. The numbers it
                  used to restate are on the pages that own them. */}
              <Route path="/" element={<Dashboard me={me} />} />
              <Route path="/sites" element={<Sites me={me} />} />
              <Route path="/sites/*" element={<Sites me={me} />} />
              <Route path="/databases" element={<Databases me={me} />} />
              <Route path="/wordpress" element={<WordPress me={me} />} />
              <Route path="/security/malware" element={<Malware me={me} />} />
              <Route path="/cron" element={<Cron me={me} />} />
              <Route path="/files" element={<Files me={me} />} />
              <Route path="/backups" element={<Backups me={me} />} />
              <Route path="/ssl" element={<Ssl me={me} />} />
              <Route path="/logs" element={<Logs />} />
              <Route path="/account" element={<Account me={me} onChanged={setMe} />} />
              {(role === 'admin' || role === 'reseller') && (
                <>
                  <Route
                    path="/users"
                    element={(
                      <Users
                        me={me}
                        onImpersonated={(u) => { setMe({ ...u, impersonated: true, impersonated_by: me.username }); navigate('/'); }}
                      />
                    )}
                  />
                  <Route path="/packages" element={<Packages me={me} />} />
                  <Route path="/import" element={<Import me={me} />} />
                </>
              )}
              {role === 'admin' && (
                <>
                  <Route path="/security/firewall" element={<Firewall />} />
                  <Route path="/security/waf" element={<Waf />} />
                  <Route path="/php" element={<Php me={me} />} />
                  <Route path="/system" element={<System />} />
                  <Route path="/settings" element={<Settings />} />
                </>
              )}
                <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>
          </main>
          <StatusRail me={me} />
        </div>
      </div>
    </div>
  );
}

function pageTitle(path) {
  if (path === '/') return 'Dashboard';
  // Longest match first, so /security/malware is not answered by /security.
  const all = FEATURES.flatMap((g) => g.items)
    .slice()
    .sort((a, b) => b.to.length - a.to.length);
  const found = all.find((i) => path === i.to || path.startsWith(i.to + '/'));
  return found ? found.label : 'Dashboard';
}
