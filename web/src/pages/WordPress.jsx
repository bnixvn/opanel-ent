import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Search, Tag, matches, useConfirm, useMessage } from '../components.jsx';

export default function WordPress({ me }) {
  const [sites, setSites] = useState([]);
  const [open, setOpen] = useState(null);
  const [query, setQuery] = useState('');
  const [loading, setLoading] = useState(true);
  const msg = useMessage();

  const staff = me.role === 'admin' || me.role === 'reseller';

  const load = useCallback(async () => {
    try {
      const res = await api.get('/wordpress');
      setSites(res.sites || []);
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

      <Card
        title="WordPress websites"
        actions={<Search value={query} onChange={setQuery} placeholder="Search domains…" />}
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
            <p className="muted" style={{ flex: 1, margin: 0, fontSize: '.82rem' }}>
              Signs in as <code>{info.admin_user || 'the administrator'}</code> with a
              link that works once and expires in two minutes. The panel has no
              copy of the WordPress password and never changes it.
            </p>
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
