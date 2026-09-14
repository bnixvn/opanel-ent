import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Search, Secret, Tag, matches, useConfirm, useMessage } from '../components.jsx';
import WordPressInstall, { useWordPressInstall } from '../WordPressInstall.jsx';

export default function Sites({ me }) {
  const [sites, setSites] = useState([]);
  const [php, setPhp] = useState([]);
  const [owners, setOwners] = useState([]);
  const [wp, setWp] = useState({});
  const [query, setQuery] = useState('');
  const [busy, setBusy] = useState(null);
  // The website waiting for its WordPress details, when the install button
  // on a row was pressed.
  const [wpFor, setWpFor] = useState(null);
  const [installed, setInstalled] = useState(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const staff = me.role === 'admin' || me.role === 'reseller';

  const load = useCallback(async () => {
    try {
      const res = await api.get('/sites');
      setSites(res.sites || []);
      return res.sites || [];
    } catch (err) {
      msg.fail(err);
      return [];
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    load().then((rows) => loadWordPress(rows));
    api.get('/php/versions').then((r) => setPhp((r.versions || []).filter((v) => v.installed))).catch(() => {});
    if (staff) api.get('/users').then((r) => setOwners(r.users || [])).catch(() => {});
  }, [load, staff]);

  // The application in a document root is a question for the host, so each
  // row asks for itself once the table is already on screen.
  function loadWordPress(rows) {
    (rows || []).filter((s) => s.app_type === 'wordpress').forEach((s) => {
      api.get(`/sites/${s.id}/wordpress`)
        .then((r) => setWp((prev) => ({ ...prev, [s.id]: r.wordpress })))
        .catch(() => {});
    });
  }

  // createSite runs the whole sequence a new website usually needs, in the
  // order that actually works: the site, then the certificate, then
  // WordPress. WordPress bakes its address into the database at install time,
  // so installing before the certificate exists means fixing the address by
  // hand afterwards.
  //
  // Each step reports on its own and a failure does not undo what came
  // before: a website whose certificate could not be issued because DNS has
  // not propagated is still a website, and the SSL page can finish the job in
  // a minute.
  async function createSite({ domain, ownerID, version, wordpress, ssl, wpDetails }) {
    msg.clear();
    setBusy('new');
    let site;
    try {
      const body = {
        domain,
        app_type: wordpress ? 'wordpress' : 'php',
        php_version: version,
        rewrite_mode: 'none',
      };
      if (staff && ownerID) body.owner_id = Number(ownerID);
      site = await api.post('/sites', body);
      msg.ok(`Created ${domain}`);
    } catch (err) {
      msg.fail(err);
      setBusy(null);
      await load();
      return null;
    }

    const notes = [`Created ${domain}`];
    if (ssl) {
      msg.ok(`${domain} created. Requesting a certificate…`);
      try {
        await api.post(`/sites/${site.id}/certificate`, { force_https: true });
        notes.push('certificate issued');
      } catch (err) {
        notes.push(`no certificate yet (${err.message})`);
      }
    }
    if (wordpress) {
      msg.ok(`${notes.join(', ')}. Installing WordPress — leave the page open.`);
      try {
        const res = await api.post(`/sites/${site.id}/wordpress`, wpDetails || {});
        setInstalled(res.wordpress);
        notes.push('WordPress installed');
      } catch (err) {
        notes.push(`WordPress not installed (${err.message})`);
      }
    }

    // One closing line listing what happened, so a half-finished setup is
    // obvious rather than something to discover later.
    const failed = notes.some((n) => n.startsWith('no ') || n.includes('not installed'));
    if (failed) msg.warn(notes.join('. ') + '.');
    else msg.ok(notes.join(', ') + '.');

    setBusy(null);
    const rows = await load();
    loadWordPress(rows);
    return site;
  }

  async function guard(fn, okText) {
    msg.clear();
    try {
      await fn();
      msg.ok(okText);
    } catch (err) {
      msg.fail(err);
    }
    const rows = await load();
    loadWordPress(rows);
  }

  const shown = sites.filter(
    (s) => matches(s.domain, query) || matches(s.owner, query) || matches((s.aliases || []).join(' '), query),
  );

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <NewSite
        php={php}
        owners={owners}
        staff={staff}
        me={me}
        onCreate={createSite}
      />

      {wpFor && (
        <InstallWordPressCard
          site={wpFor}
          onCancel={() => setWpFor(null)}
          onDone={async (fn) => {
            setBusy(wpFor.id);
            msg.ok(`Installing WordPress on ${wpFor.domain}. Leave the page open.`);
            try {
              const res = await fn();
              setInstalled(res.wordpress);
              setWpFor(null);
              msg.clear();
            } catch (err) {
              msg.fail(err);
            }
            setBusy(null);
            const rows = await load();
            loadWordPress(rows);
          }}
        />
      )}

      {installed && (
        <Card title="WordPress is installed">
          <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
            The administrator password is shown once. The panel keeps no copy it
            can read back.
          </p>
          <dl className="kv">
            <dt>Site</dt>
            <dd><a href={installed.site_url} target="_blank" rel="noreferrer">{installed.site_url}</a></dd>
            <dt>Dashboard</dt>
            <dd><a href={installed.admin_url} target="_blank" rel="noreferrer">{installed.admin_url}</a></dd>
            <dt>Database</dt>
            <dd><code>{installed.database}</code></dd>
          </dl>
          <Secret label="Administrator" value={installed.admin_user} />
          <Secret label="Password" value={installed.admin_password} />
          <button type="button" onClick={() => setInstalled(null)}>Close</button>
        </Card>
      )}

      <Card
        title="Websites"
        actions={<Search value={query} onChange={setQuery} placeholder="Search domains…" />}
      >
        {shown.length === 0 ? (
          <Empty>{sites.length ? 'Nothing matches that search.' : 'No websites yet.'}</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>Domain</th>
                  {staff && <th>Owner</th>}
                  <th>Type</th>
                  <th>PHP</th>
                  <th>SSL</th>
                  <th>App</th>
                  <th>Status</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {shown.map((s) => (
                  <tr key={s.id}>
                    <td>
                      <code>{s.domain}</code>
                      {(s.aliases || []).length > 0 && (
                        <div className="muted" style={{ fontSize: '.8em' }}>{s.aliases.join(', ')}</div>
                      )}
                    </td>
                    {staff && (
                      <td>
                        <select
                          value={s.owner_id}
                          onChange={async (e) => {
                            const to = owners.find((u) => u.id === Number(e.target.value));
                            if (!to) return;
                            const ok = await ask({
                              title: `Move ${s.domain} to ${to.username}?`,
                              body:
                                `The website's files move with it, from `
                                + `/home/${s.owner}/${s.domain} to /home/${to.username}/${s.domain}. `
                                + 'The site keeps serving throughout.\n\n'
                                + 'Databases are not moved: their names carry the owner as a '
                                + 'prefix, so renaming them would break whatever connects to them.',
                              confirmLabel: 'Move it',
                            });
                            if (!ok) { await load(); return; }
                            msg.ok(`Moving ${s.domain}. A large site takes a moment.`);
                            try {
                              const res = await api.post(`/sites/${s.id}/owner`, {
                                owner_id: to.id,
                              });
                              msg.ok(res.note
                                ? `${s.domain} now belongs to ${to.username}. ${res.note}`
                                : `${s.domain} now belongs to ${to.username}`);
                            } catch (err) {
                              msg.fail(err);
                            }
                            const rows = await load();
                            loadWordPress(rows);
                          }}
                        >
                          {owners.filter((u) => u.linux_uid).map((u) => (
                            <option key={u.id} value={u.id}>{u.username}</option>
                          ))}
                          {!owners.some((u) => u.id === s.owner_id) && (
                            <option value={s.owner_id}>{s.owner}</option>
                          )}
                        </select>
                      </td>
                    )}
                    <td>
                      <select
                        value={s.app_type}
                        onChange={async (e) => {
                          const to = e.target.value;
                          const ok = await ask({
                            title: `Change ${s.domain} to ${to}?`,
                            body: to === 'static'
                              ? 'A static site runs no PHP at all. Anything that '
                                + 'depends on it stops working until you change back.'
                              : 'This only changes how the webserver treats the site: '
                                + 'the rewrite rules and whether PHP runs. No files are '
                                + 'touched.',
                            confirmLabel: 'Change it',
                          });
                          if (!ok) { await load(); return; }
                          guard(
                            () => api.patch(`/sites/${s.id}`, { app_type: to }),
                            `${s.domain} is now ${to}`,
                          );
                        }}
                      >
                        <option value="wordpress">wordpress</option>
                        <option value="php">php</option>
                        <option value="static">static</option>
                      </select>
                    </td>
                    <td>
                      {s.app_type === 'static' ? (
                        <span className="muted">—</span>
                      ) : (
                        <>
                          <select
                            value={s.php_version}
                            onChange={(e) =>
                              guard(
                                () => api.patch(`/sites/${s.id}`, { php_version: e.target.value }),
                                `PHP set to ${e.target.value}`,
                              )}
                          >
                            {php.map((v) => <option key={v.version}>{v.version}</option>)}
                          </select>
                        </>
                      )}
                    </td>
                    <td><SslCell site={s} guard={guard} ask={ask} msg={msg} /></td>
                    <td>
                      <WordPressCell
                        site={s}
                        status={wp[s.id]}
                        busy={busy === s.id}
                        onInstall={() => setWpFor(s)}
                      />
                    </td>
                    <td>
                      {s.suspended ? <Tag kind="warn">suspended</Tag> : <Tag kind="ok">active</Tag>}
                    </td>
                    <td className="right nowrap">
                      <button
                        type="button"
                        onClick={() =>
                          guard(
                            () => api.patch(`/sites/${s.id}`, { suspended: !s.suspended }),
                            s.suspended ? 'Site unsuspended' : 'Site suspended',
                          )}
                      >
                        {s.suspended ? 'Unsuspend' : 'Suspend'}
                      </button>{' '}
                      <button
                        type="button"
                        className="danger"
                        onClick={async () => {
                          const ok = await ask({
                            title: `Delete ${s.domain}?`,
                            body: "This also deletes the site's files and cannot be undone.",
                            confirmLabel: 'Delete',
                            danger: true,
                          });
                          if (ok) guard(() => api.del(`/sites/${s.id}?remove_files=true`), 'Site deleted');
                        }}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}

function SslCell({ site, guard, ask, msg }) {
  if (!site.ssl_enabled) {
    return (
      <button
        type="button"
        onClick={async () => {
          const ok = await ask({
            title: `Get a certificate for ${site.domain}?`,
            body:
              'The domain must already point at this server — the certificate '
              + 'authority fetches a file over plain HTTP to check.\n\n'
              + 'HTTP will be redirected to HTTPS afterwards.',
            confirmLabel: 'Request certificate',
          });
          if (!ok) return;
          msg.ok(`Requesting a certificate for ${site.domain}. This can take a minute.`);
          guard(
            () => api.post(`/sites/${site.id}/certificate`, { force_https: true }),
            `HTTPS is on for ${site.domain}`,
          );
        }}
      >
        Enable SSL
      </button>
    );
  }
  const days = site.cert_expires
    ? Math.round((new Date(site.cert_expires) - Date.now()) / 86400000)
    : null;
  const kind = days === null ? 'ok' : days < 0 ? 'bad' : days < 15 ? 'warn' : 'ok';
  return (
    <>
      <Tag kind={kind}>{days === null ? 'on' : `${days}d left`}</Tag>
      {site.force_https && <> <Tag>forced</Tag></>}
      <div>
        <button
          type="button"
          className="link"
          style={{ fontSize: '.8em' }}
          onClick={() => guard(() => api.del(`/sites/${site.id}/certificate`), 'HTTPS turned off')}
        >
          turn off
        </button>
      </div>
    </>
  );
}

function WordPressCell({ site, status, busy, onInstall }) {
  if (site.app_type !== 'wordpress') return <span className="muted">—</span>;
  if (!status) return <span className="muted">…</span>;
  if (status.installed) {
    return (
      <>
        <Tag kind="ok">WP {status.version}</Tag>
        <div>
          <a
            href={`${site.ssl_enabled ? 'https' : 'http'}://${site.domain}/wp-admin/`}
            target="_blank"
            rel="noreferrer"
            style={{ fontSize: '.8em' }}
          >
            wp-admin
          </a>
        </div>
      </>
    );
  }
  if (!status.cli_ready) return <span className="muted" style={{ fontSize: '.85em' }}>WP-CLI missing</span>;
  if (!status.empty) return <span className="muted" style={{ fontSize: '.85em' }}>has files</span>;
  return (
    <button type="button" disabled={busy} onClick={onInstall}>
      {busy ? 'Installing…' : 'Install WordPress'}
    </button>
  );
}

function NewSite({ php, owners, staff, me, onCreate }) {
  const wp = useWordPressInstall();
  const [domain, setDomain] = useState('');
  const [version, setVersion] = useState('');
  const [owner, setOwner] = useState('');
  const [wordpress, setWordpress] = useState(false);
  const [ssl, setSsl] = useState(true);
  const [busy, setBusy] = useState(false);

  // Only accounts with a home directory can own a website, because that is
  // where its files go. Staff accounts have none, so offering "me" to an
  // administrator was offering a choice that always failed.
  const eligible = owners.filter((u) => u.linux_uid);

  useEffect(() => {
    if (!version && php.length) setVersion(php[php.length - 1].version);
  }, [php, version]);

  useEffect(() => {
    if (staff && !owner && eligible.length) setOwner(String(eligible[0].id));
  }, [staff, owner, eligible]);

  const ownerName = staff
    ? ((eligible.find((u) => String(u.id) === String(owner)) || {}).username || '')
    : me.username;

  return (
    <Card title="New website">
      <form
        className="row"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          const created = await onCreate({
            domain: domain.trim(),
            ownerID: owner,
            version,
            wordpress,
            ssl,
            wpDetails: wordpress ? wp.body() : null,
          });
          setBusy(false);
          if (created) { setDomain(''); wp.reset(); }
        }}
      >
        <div className="field">
          <label htmlFor="nsDomain">Domain</label>
          <input
            id="nsDomain"
            placeholder="example.com"
            required
            value={domain}
            onChange={(e) => setDomain(e.target.value)}
          />
        </div>
        {staff && (
          <div className="field" style={{ flex: '0 0 11rem' }}>
            <label htmlFor="nsOwner">Owner</label>
            <select
              id="nsOwner"
              value={owner}
              required
              onChange={(e) => setOwner(e.target.value)}
            >
              {eligible.length === 0 && <option value="">no hosting accounts yet</option>}
              {eligible.map((u) => <option key={u.id} value={u.id}>{u.username}</option>)}
            </select>
          </div>
        )}
        <div className="field" style={{ flex: '0 0 7rem' }}>
          <label htmlFor="nsPhp">PHP</label>
          <select id="nsPhp" value={version} onChange={(e) => setVersion(e.target.value)}>
            {php.map((v) => <option key={v.version}>{v.version}</option>)}
          </select>
        </div>
        <div>
          <button
            type="submit"
            className="primary"
            disabled={busy || (staff && eligible.length === 0)}
          >
            {busy ? 'Working…' : 'Create'}
          </button>
        </div>
        <div className="checks" style={{ flexBasis: '100%', gap: '.2rem 1.4rem' }}>
          <label className="check">
            <input
              type="checkbox"
              checked={ssl}
              onChange={(e) => setSsl(e.target.checked)}
            />
            Get an SSL certificate
          </label>
          <label className="check">
            <input
              type="checkbox"
              checked={wordpress}
              onChange={(e) => setWordpress(e.target.checked)}
            />
            Install WordPress
          </label>
        </div>
        {wordpress && (
          <div style={{ flexBasis: '100%' }}>
            <WordPressInstall
              embedded
              domain={domain.trim()}
              owner={ownerName}
              value={wp.value}
              onChange={wp.setValue}
            />
          </div>
        )}
        <p className="muted" style={{ fontSize: '.83rem', flexBasis: '100%', margin: '.4rem 0 0' }}>
          {staff && eligible.length === 0 ? (
            <>Create a hosting account under Users first — a website&apos;s files
              live in its owner&apos;s home directory, and staff accounts have none.</>
          ) : (
            <>
              Files go to{' '}
              <code>/home/{ownerName}/{domain.trim() || 'example.com'}/public_html</code>.
              {ssl && ' The certificate needs this domain already pointing at this server;'}
              {ssl && ' if it does not yet, the website is still created and the SSL page can issue one later.'}
            </>
          )}
        </p>
      </form>
    </Card>
  );
}


// InstallWordPressCard asks for the details before it installs.
//
// A dialog rather than a confirmation, because everything here ends up baked
// into the site's database: changing the administrator's name afterwards
// means editing WordPress, and changing the address means editing it in two
// places.
function InstallWordPressCard({ site, onDone, onCancel }) {
  const wp = useWordPressInstall();
  const [busy, setBusy] = useState(false);

  return (
    <Card
      title={<>Install WordPress on <code>{site.domain}</code></>}
      actions={<button type="button" onClick={onCancel} disabled={busy}>Cancel</button>}
    >
      <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
        {!site.ssl_enabled && (
          <>No certificate yet — WordPress will be set up on{' '}
            <code>http://{site.domain}</code>.</>
        )}
      </p>

      <WordPressInstall
        domain={site.domain}
        owner={site.owner}
        value={wp.value}
        onChange={wp.setValue}
      />

      <p style={{ marginBottom: 0, marginTop: '.7rem' }}>
        <button
          type="button"
          className="primary"
          disabled={busy}
          onClick={() => {
            setBusy(true);
            onDone(() => api.post(`/sites/${site.id}/wordpress`, wp.body()));
          }}
        >
          {busy ? 'Installing…' : 'Install WordPress'}
        </button>
      </p>
    </Card>
  );
}
