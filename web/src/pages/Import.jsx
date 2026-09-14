import React, { useEffect, useRef, useState } from 'react';
import { api, fmtBytes } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

// Import brings a cPanel or DirectAdmin account onto this server.
//
// Two steps on purpose. The archive is read first and nothing is created,
// because somebody moving a customer needs to see the domains, the databases
// and — more importantly — what is not coming with them, before the DNS is
// cut over rather than a week afterwards.
export default function Import({ me }) {
  const [owners, setOwners] = useState([]);
  const [owner, setOwner] = useState('');
  const [staged, setStaged] = useState(null);
  const [plan, setPlan] = useState(null);
  const [pickedDomains, setPickedDomains] = useState([]);
  const [pickedDBs, setPickedDBs] = useState([]);
  const [report, setReport] = useState(null);
  const [busy, setBusy] = useState(false);
  const fileInput = useRef(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  useEffect(() => {
    api.get('/users').then((r) => {
      const list = (r.users || []).filter((u) => u.linux_uid);
      setOwners(list);
      if (list.length) setOwner(list[0].username);
    }).catch(() => {});
  }, []);

  async function upload(e) {
    e.preventDefault();
    const f = fileInput.current && fileInput.current.files[0];
    if (!f) return;
    msg.clear();
    setBusy(true);
    setReport(null);
    try {
      const body = new FormData();
      body.append('file', f, f.name);
      const res = await api.post('/import/upload', body);
      setStaged(res.staged);
      setPlan(res.plan);
      setPickedDomains((res.plan.domains || []).map((d) => d.name));
      setPickedDBs((res.plan.databases || []).map((d) => d.name));
      msg.ok(`Read a ${res.plan.format === 'cpanel' ? 'cPanel' : 'DirectAdmin'} backup of ${fmtBytes(res.size)}.`);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
  }

  async function discard() {
    if (staged) {
      try { await api.del(`/import?staged=${encodeURIComponent(staged)}`); } catch { /* it goes on its own */ }
    }
    setStaged(null);
    setPlan(null);
    setReport(null);
    if (fileInput.current) fileInput.current.value = '';
  }

  async function run() {
    const ok = await ask({
      title: `Import into ${owner}?`,
      body: `This creates ${pickedDomains.length} website(s) and ${pickedDBs.length} `
        + `database(s) under ${owner}, and unpacks the files into them. `
        + 'Nothing already on this server is overwritten except a file of the '
        + 'same name in the same place. It can take a while on a large account.',
      confirmLabel: 'Import',
    });
    if (!ok) return;
    msg.clear();
    setBusy(true);
    try {
      const res = await api.post('/import/run', {
        staged, owner, domains: pickedDomains, databases: pickedDBs,
      });
      setReport(res.report);
      msg.ok('Finished. Check what it says below.');
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
  }

  const toggle = (list, set) => (name) => set(
    list.includes(name) ? list.filter((n) => n !== name) : [...list, name],
  );

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card title="Import an account">
        <form className="row" onSubmit={upload}>
          <div className="field">
            <label htmlFor="impFile">Backup archive</label>
            <input id="impFile" type="file" accept=".gz,.tgz" ref={fileInput} />
          </div>
          <div>
            <button type="submit" className="primary" disabled={busy}>
              {busy && !plan ? 'Reading…' : 'Read the archive'}
            </button>{' '}
            {plan && <button type="button" onClick={discard} disabled={busy}>Start over</button>}
          </div>
        </form>
      </Card>

      {plan && !report && (
        <Card title="What is in it">
          <dl className="kv">
            <dt>Format</dt>
            <dd>{plan.format === 'cpanel' ? 'cPanel' : 'DirectAdmin'}</dd>
            <dt>Account there</dt>
            <dd>{plan.source_user || <span className="muted">not recorded</span>}</dd>
            <dt>Files</dt>
            <dd className="muted">{plan.home_files} files, {fmtBytes(plan.home_bytes)}</dd>
          </dl>

          {(plan.warnings || []).map((warn) => (
            <p className="msg warn" key={warn}>{warn}</p>
          ))}

          <Picker
            title="Websites"
            rows={(plan.domains || []).map((d) => ({
              name: d.name,
              detail: [d.main ? 'main domain' : d.kind, d.php_version && `PHP ${d.php_version}`]
                .filter(Boolean).join(' · '),
            }))}
            picked={pickedDomains}
            onToggle={toggle(pickedDomains, setPickedDomains)}
            empty="No websites were found in this archive."
          />

          <Picker
            title="Databases"
            rows={(plan.databases || []).map((d) => ({
              name: d.name, detail: fmtBytes(d.bytes),
            }))}
            picked={pickedDBs}
            onToggle={toggle(pickedDBs, setPickedDBs)}
            empty="No databases were found in this archive."
            note="Databases are renamed to this panel's prefix, so two accounts
              moving from the same server cannot collide. Update the
              application's configuration afterwards."
          />

          <NotImported items={plan.not_imported} />

          <div className="row" style={{ marginTop: '.9rem', alignItems: 'flex-end' }}>
            <div className="field" style={{ flex: '0 0 14rem' }}>
              <label htmlFor="impOwner">Import into</label>
              <select id="impOwner" value={owner} onChange={(e) => setOwner(e.target.value)}>
                {owners.length === 0 && <option value="">no hosting accounts yet</option>}
                {owners.map((u) => <option key={u.id}>{u.username}</option>)}
              </select>
            </div>
            <div>
              <button
                type="button"
                className="primary"
                disabled={busy || !owner || (pickedDomains.length === 0 && pickedDBs.length === 0)}
                onClick={run}
              >
                {busy ? 'Importing — leave the page open…' : 'Import'}
              </button>
            </div>
          </div>
        </Card>
      )}

      {report && (
        <Card title="What happened">
          <dl className="kv">
            <dt>Into</dt><dd>{report.owner}</dd>
            <dt>Websites</dt>
            <dd>{report.sites && report.sites.length
              ? report.sites.map((d) => <Tag kind="ok" key={d}>{d}</Tag>)
              : <span className="muted">none</span>}</dd>
            <dt>Databases</dt>
            <dd>{report.databases && report.databases.length
              ? report.databases.map((d) => <Tag kind="ok" key={d}>{d}</Tag>)
              : <span className="muted">none</span>}</dd>
            <dt>Files</dt>
            <dd className="muted">{report.files} files, {fmtBytes(report.bytes)}</dd>
          </dl>

          {(report.failed || []).length > 0 && (
            <>
              <p style={{ margin: '0 0 .3rem', fontWeight: 600, fontSize: '.9rem' }}>
                What did not work
              </p>
              <ul className="msg err" style={{ paddingLeft: '1.3rem' }}>
                {report.failed.map((f) => <li key={f}>{f}</li>)}
              </ul>
            </>
          )}
          {(report.notes || []).map((n) => <p className="msg warn" key={n}>{n}</p>)}

          <NotImported items={report.not_imported} />

          <button type="button" onClick={discard}>Done</button>
        </Card>
      )}
    </>
  );
}

function Picker({ title, rows, picked, onToggle, empty, note }) {
  return (
    <>
      <p style={{ margin: '.9rem 0 .3rem', fontWeight: 600, fontSize: '.9rem' }}>{title}</p>
      {rows.length === 0 ? <Empty>{empty}</Empty> : (
        <div className="checks" style={{ flexDirection: 'column', gap: '.25rem' }}>
          {rows.map((row) => (
            <label className="check" key={row.name}>
              <input
                type="checkbox"
                checked={picked.includes(row.name)}
                onChange={() => onToggle(row.name)}
              />
              <code>{row.name}</code>
              {row.detail && <span className="muted" style={{ fontSize: '.8rem' }}>{row.detail}</span>}
            </label>
          ))}
        </div>
      )}
      {note && <p className="muted" style={{ fontSize: '.8rem', margin: '.3rem 0 0' }}>{note}</p>}
    </>
  );
}

// NotImported is deliberately prominent. The failure mode this page exists to
// prevent is somebody moving a customer, cutting the DNS over, and finding a
// week later that the email never came with them.
function NotImported({ items }) {
  if (!items || items.length === 0) return null;
  return (
    <div className="msg warn" style={{ marginTop: '.9rem' }}>
      <strong>Not brought across</strong>
      <ul style={{ margin: '.3rem 0 0', paddingLeft: '1.2rem' }}>
        {items.map((i) => <li key={i}>{i}</li>)}
      </ul>
    </div>
  );
}
