import React, { useCallback, useEffect, useState } from 'react';
import { NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { api, setUnauthorizedHandler } from './api.js';
import Login from './Login.jsx';
import Sites from './pages/Sites.jsx';
import Databases from './pages/Databases.jsx';
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

// The menu. `roles` lists who sees an entry; the API enforces the same rule,
// so hiding one is a convenience and never the protection.
const MENU = [
  { group: '', items: [
    { to: '/', label: 'Websites', end: true },
    { to: '/databases', label: 'Databases' },
    { to: '/files', label: 'File manager' },
    { to: '/backups', label: 'Backups' },
    { to: '/ssl', label: 'SSL' },
    { to: '/logs', label: 'Logs' },
  ] },
  { group: 'Security', roles: ['admin'], items: [
    { to: '/security/firewall', label: 'Firewall', roles: ['admin'] },
    { to: '/security/waf', label: 'WAF', roles: ['admin'] },
  ] },
  { group: 'Server', roles: ['admin', 'reseller'], items: [
    { to: '/users', label: 'Users', roles: ['admin', 'reseller'] },
    { to: '/packages', label: 'Packages', roles: ['admin', 'reseller'] },
    { to: '/php', label: 'PHP', roles: ['admin'] },
    { to: '/system', label: 'System', roles: ['admin'] },
    { to: '/settings', label: 'Settings', roles: ['admin'] },
  ] },
  { group: 'You', items: [
    { to: '/account', label: 'Account' },
  ] },
];

function visible(entry, role) {
  return !entry.roles || entry.roles.includes(role);
}

export default function App() {
  const [me, setMe] = useState(null);
  const [brand, setBrand] = useState({ name: 'OPanel' });
  const [loading, setLoading] = useState(true);
  const [quota, setQuota] = useState(null);
  const location = useLocation();

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
  }, [me]);

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

        {MENU.filter((section) => visible(section, role)).map((section) => {
          const items = section.items.filter((i) => visible(i, role));
          if (!items.length) return null;
          return (
            <div key={section.group || 'top'}>
              {section.group && <div className="group">{section.group}</div>}
              {items.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) => (isActive ? 'active' : undefined)}
                >
                  {item.label}
                </NavLink>
              ))}
            </div>
          );
        })}

        <div className="spacer" />
        <div className="who">
          {me.username}
          <br />
          <span style={{ opacity: 0.7 }}>{role}</span>
        </div>
        <button
          type="button"
          style={{ margin: '.5rem .6rem' }}
          onClick={async () => {
            try { await api.post('/auth/logout'); } catch { /* signing out anyway */ }
            signOut();
          }}
        >
          Sign out
        </button>
      </nav>

      <div className="main">
        {quota && !quota.enforced && role === 'admin' && (
          <div className="banner">
            {quota.pending_reboot
              ? 'Disk quotas are configured but not active yet — reboot the server to start enforcing them.'
              : 'Disk quotas are not enforced on this server. Package disk limits are advisory only.'}
          </div>
        )}

        <header className="topbar">
          <h1>{title}</h1>
        </header>

        <main className="content">
          <Routes>
            {/* Websites is the landing page: it is what an operator opens
                the panel to look at, and a dashboard that only restated the
                same numbers was a click in the way. */}
            <Route path="/" element={<Sites me={me} />} />
            <Route path="/sites/*" element={<Sites me={me} />} />
            <Route path="/databases" element={<Databases me={me} />} />
            <Route path="/files" element={<Files me={me} />} />
            <Route path="/backups" element={<Backups me={me} />} />
            <Route path="/ssl" element={<Ssl me={me} />} />
            <Route path="/logs" element={<Logs />} />
            <Route path="/account" element={<Account me={me} />} />
            {(role === 'admin' || role === 'reseller') && (
              <>
                <Route path="/users" element={<Users me={me} />} />
                <Route path="/packages" element={<Packages me={me} />} />
              </>
            )}
            {role === 'admin' && (
              <>
                <Route path="/security/firewall" element={<Firewall />} />
                <Route path="/security/waf" element={<Waf />} />
                <Route path="/php" element={<Php />} />
                <Route path="/system" element={<System />} />
                <Route path="/settings" element={<Settings />} />
              </>
            )}
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </main>
      </div>
    </div>
  );
}

function pageTitle(path) {
  const found = MENU.flatMap((s) => s.items).find(
    (i) => (i.end ? path === i.to : path.startsWith(i.to)),
  );
  return found ? found.label : 'Websites';
}
