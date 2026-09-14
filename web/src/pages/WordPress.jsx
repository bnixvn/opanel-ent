import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Search, Secret, Tag, matches, useConfirm, useMessage } from '../components.jsx';
import WordPressInstall, { useWordPressInstall } from '../WordPressInstall.jsx';

export default function WordPress({ me }) {
  const [sites, setSites] = useState([]);
  const [open, setOpen] = useState(null);
  const [query, setQuery] = useState('');
  const [loading, setLoading] = useState(true);
  // Websites with no WordPress yet, so one can be installed from here rather
  // than from the websites page.
  const [candidates, setCandidates] = useState([]);
  const [installing, setInstalling] = useState(false);
  const [installed, setInstalled] = useState(null);
  const msg = useMessage();

  const staff = me.role === 'admin' || me.role === 'reseller';

  const load = useCallback(async () => {
    try {
      const res = await api.get('/wordpress');
      setSites(res.sites || []);

      // Which websites could take one. A site is a candidate when it runs
      // PHP and has nothing in its document root yet; the panel asks the
      // disk rather than trusting the sites table, because a customer can
      // put files there over SFTP without telling it.
      const all = (await api.get('/sites')).sites || [];
      const withWP = new Set((res.sites || []).map((x) => x.site_id));
      const open = [];
      for (const site of all) {
        if (withWP.has(site.id) || site.app_type === 'static' || site.suspended) continue;
        try {
          const st = await api.get(`/sites/${site.id}/wordpress`);
          if (st.wordpress && st.wordpress.empty && st.wordpress.cli_ready) open.push(site);
        } catch { /* a site the caller cannot reach is not a candidate */ }
      }
      setCandidates(open);
    } catch (err) {
      msg.fail(err);
    }
    setLoading(false);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  const shown = sites.filter((s) => matches(s.domain, query) || matches(s.owner, query));

  return (
    <>
      <Message value={msg.message} onClear={msg.clear} />

      {installed && (
        <Card title="WordPress is installed">
          <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
            The administrator password is shown once. The panel keeps no copy
            it can read back.
          </p>
          <dl className="kv">
            <dt>Site</dt>
            <dd><a href={installed.site_url} target="_blank" rel="noreferrer">{installed.site_url}</a></dd>
            <dt>Dashboard</dt>
            <dd><a href={installed.admin_url} target="_blank" rel="noreferrer">{installed.admin_url}</a></dd>
            <dt>Database</dt><dd><code>{installed.database}</code></dd>
          </dl>
          <Secret label="Administrator" value={installed.admin_user} />
          <Secret label="Password" value={installed.admin_password} />
          <button type="button" onClick={() => setInstalled(null)}>Close</button>
        </Card>
      )}

      {installing && (
        <InstallHere
          candidates={candidates}
          onCancel={() => setInstalling(false)}
          onDone={(wordpress) => { setInstalling(false); setInstalled(wordpress); load(); }}
          onFailed={(err) => msg.fail(err)}
        />
      )}

      <Card
        title="WordPress websites"
        actions={(
          <>
            {candidates.length > 0 && !installing && (
              <>
                <button type="button" className="primary" onClick={() => setInstalling(true)}>
                  Install WordPress
                </button>{' '}
              </>
            )}
            <Search value={query} onChange={setQuery} placeholder="Search domains…" />
          </>
        )}
      >
        {loading ? <p className="muted">Looking at every website…</p> : shown.length === 0 ? (
          <Empty>
            {sites.length
              ? 'Nothing matches that search.'
              : 'No WordPress installations found. Tick "Install WordPress" when you create a website.'}
          </Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>Website</th>
                  {staff && <th>Owner</th>}
                  <th>Version</th>
                  <th>PHP</th>
                  <th>Status</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {shown.map((s) => (
                  <tr key={s.site_id}>
                    <td>
                      <code>{s.domain}</code>{' '}
                      {s.ssl ? <Tag kind="ok">https</Tag> : <Tag kind="warn">no ssl</Tag>}
                    </td>
                    {staff && <td className="muted">{s.owner}</td>}
                    <td>{s.version || <span className="muted">unknown</span>}</td>
                    <td className="muted">{s.php}</td>
                    <td>
                      {s.suspended
                        ? <Tag kind="warn">suspended</Tag>
                        : <Tag kind="ok">active</Tag>}
                    </td>
                    <td className="right nowrap">
                      <button
                        type="button"
                        className="link"
                        onClick={() => setOpen(open && open.site_id === s.site_id ? null : s)}
                      >
                        {open && open.site_id === s.site_id ? 'Close' : 'Manage'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {open && <Manage site={open} onClose={() => setOpen(null)} onChanged={load} />}
    </>
  );
}

// Manage is one installation: core, plugins, themes, and the way in.
function Manage({ site, onClose, onChanged }) {
  const [info, setInfo] = useState(null);
  const [busy, setBusy] = useState(null);
  const [output, setOutput] = useState('');
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(async () => {
    setInfo(null);
    try {
      const res = await api.get(`/sites/${site.site_id}/wordpress/info`);
      setInfo(res.info);
    } catch (err) {
      msg.fail(err);
    }
  }, [site.site_id]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  async function run(label, body, confirmation) {
    if (confirmation) {
      const ok = await ask(confirmation);
      if (!ok) return;
    }
    msg.clear();
    setOutput('');
    setBusy(label);
    try {
      const res = await api.post(`/sites/${site.site_id}/wordpress/manage`, body);
      if (res.info) setInfo(res.info);
      setOutput(res.output || '');
      msg.ok(`${label} finished.`);
      onChanged();
    } catch (err) {
      msg.fail(err);
    }
    setBusy(null);
  }

  async function signOn() {
    msg.clear();
    setBusy('sign-in');
    try {
      const res = await api.post(`/sites/${site.site_id}/wordpress/signon`, {});
      window.open(res.url, '_blank', 'noopener');
    } catch (err) {
      msg.fail(err);
    }
    setBusy(null);
  }

  const pluginUpdates = (info?.plugins || []).filter((p) => p.update).length;
  const themeUpdates = (info?.themes || []).filter((t) => t.update).length;

  return (
    <Card
      title={<>WordPress — <code>{site.domain}</code></>}
      actions={<button type="button" onClick={onClose}>Close</button>}
    >
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      {!info ? <p className="muted">Reading the installation…</p> : info.error ? (
        <p className="msg err" style={{ marginBottom: 0 }}>
          WordPress is on disk but the panel could not talk to it: {info.error}
        </p>
      ) : (
        <>
          <div className="row" style={{ alignItems: 'center', marginBottom: '.6rem' }}>
            <div>
              <button type="button" className="primary" disabled={busy} onClick={signOn}>
                {busy === 'sign-in' ? 'Opening…' : 'Open wp-admin'}
              </button>
            </div>
          </div>

          <dl className="kv">
            <dt>Core</dt>
            <dd>
              {info.version}{' '}
              {info.core_update ? (
                <>
                  <Tag kind="warn">{info.core_update} available</Tag>{' '}
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => run('Core update', { kind: 'core', op: 'update' }, {
                      title: `Update WordPress to ${info.core_update}?`,
                      body: 'Core files and the database are both updated. Take a backup '
                        + 'first if this site matters — the panel does not roll this back.',
                      confirmLabel: 'Update',
                    })}
                  >
                    {busy === 'Core update' ? 'Updating…' : 'Update core'}
                  </button>
                </>
              ) : <Tag kind="ok">up to date</Tag>}
            </dd>
            <dt>Address</dt>
            <dd>
              <a href={info.home_url || info.site_url} target="_blank" rel="noreferrer">
                {info.home_url || info.site_url}
              </a>
            </dd>
            {info.db_name && <><dt>Database</dt><dd><code>{info.db_name}</code></dd></>}
          </dl>

          <Components
            kind="plugin"
            title="Plugins"
            rows={info.plugins || []}
            updates={pluginUpdates}
            busy={busy}
            onRun={run}
          />
          <Components
            kind="theme"
            title="Themes"
            rows={info.themes || []}
            updates={themeUpdates}
            busy={busy}
            onRun={run}
          />

          {output && (
            <>
              <p style={{ margin: '.8rem 0 .2rem', fontWeight: 600, fontSize: '.85rem' }}>
                What WP-CLI said
              </p>
              <pre className="log">{output}</pre>
            </>
          )}
        </>
      )}
    </Card>
  );
}

function Components({ kind, title, rows, updates, busy, onRun }) {
  if (rows.length === 0) return null;
  const label = `All ${kind}s`;
  return (
    <>
      <div className="toolbar" style={{ marginTop: '.9rem', marginBottom: '.3rem' }}>
        <strong style={{ fontSize: '.9rem' }}>{title}</strong>
        <span className="muted" style={{ fontSize: '.82rem' }}>
          {rows.length} installed{updates ? `, ${updates} with an update` : ''}
        </span>
        <span className="grow" />
        {updates > 0 && (
          <button
            type="button"
            disabled={busy}
            onClick={() => onRun(label, { kind, op: 'update' }, {
              title: `Update ${updates} ${kind}(s)?`,
              body: 'An update can change how a site behaves. Take a backup first '
                + 'if this site matters.',
              confirmLabel: 'Update them',
            })}
          >
            {busy === label ? 'Updating…' : `Update all ${kind}s`}
          </button>
        )}
      </div>
      <div className="scroll">
        <table>
          <tbody>
            {rows.map((c) => (
              <tr key={c.name}>
                <td>
                  {c.title || c.name}
                  <div className="muted" style={{ fontSize: '.78rem' }}><code>{c.name}</code></div>
                </td>
                <td className="muted nowrap">{c.version}</td>
                <td className="nowrap">
                  {c.status === 'active' ? <Tag kind="ok">active</Tag>
                    : c.status === 'inactive' ? <Tag>inactive</Tag>
                      : <Tag kind="mute">{c.status}</Tag>}
                </td>
                <td className="right nowrap">
                  {c.update && (
                    <>
                      <button
                        type="button"
                        className="link"
                        disabled={busy}
                        onClick={() => onRun(`${c.name} update`, { kind, op: 'update', name: c.name })}
                      >
                        Update to {c.update}
                      </button>{' '}
                    </>
                  )}
                  {kind === 'plugin' && c.status !== 'must-use' && c.status !== 'dropin' && (
                    <button
                      type="button"
                      className="link"
                      disabled={busy}
                      onClick={() => onRun(
                        `${c.name} ${c.status === 'active' ? 'deactivate' : 'activate'}`,
                        { kind, op: c.status === 'active' ? 'deactivate' : 'activate', name: c.name },
                      )}
                    >
                      {c.status === 'active' ? 'Deactivate' : 'Activate'}
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}


// InstallHere installs WordPress onto a website that has none.
//
// Here rather than only on the websites page, because this is where somebody
// managing WordPress is already looking. The list is websites that can take
// one: running PHP, not suspended, and with an empty document root.
function InstallHere({ candidates, onDone, onCancel, onFailed }) {
  const [siteID, setSiteID] = useState(String(candidates[0] ? candidates[0].id : ''));
  const [busy, setBusy] = useState(false);
  const wp = useWordPressInstall();

  const site = candidates.find((c) => String(c.id) === String(siteID)) || {};

  return (
    <Card
      title="Install WordPress"
      actions={<button type="button" onClick={onCancel} disabled={busy}>Cancel</button>}
    >
      <div className="row">
        <div className="field">
          <label htmlFor="wpSite">Website</label>
          <select id="wpSite" value={siteID} onChange={(e) => setSiteID(e.target.value)}>
            {candidates.map((c) => (
              <option key={c.id} value={c.id}>
                {c.domain}{c.ssl_enabled ? '' : '  (no certificate yet)'}
              </option>
            ))}
          </select>
        </div>
      </div>

      {site.id && !site.ssl_enabled && (
        <p className="msg warn">
          {site.domain} has no certificate, so WordPress will be set up on
          http://{site.domain}. Getting the certificate first is easier than
          changing the address inside WordPress afterwards.
        </p>
      )}

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
          disabled={busy || !siteID}
          onClick={async () => {
            setBusy(true);
            try {
              const res = await api.post(`/sites/${siteID}/wordpress`, wp.body());
              onDone(res.wordpress);
            } catch (err) {
              onFailed(err);
            }
            setBusy(false);
          }}
        >
          {busy ? 'Installing — leave the page open…' : 'Install WordPress'}
        </button>
      </p>
    </Card>
  );
}
