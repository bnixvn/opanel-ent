import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api, download, fmtBytes, fmtDate } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';
import Destinations from './Destinations.jsx';

export default function Backups({ me }) {
  const [owner, setOwner] = useState('');
  const [owners, setOwners] = useState([]);
  const [rows, setRows] = useState([]);
  const [limit, setLimit] = useState(0);
  const [schedule, setSchedule] = useState(null);
  const [includeFiles, setIncludeFiles] = useState(true);
  const [includeDatabases, setIncludeDatabases] = useState(true);
  const archive = useRef(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const staff = me.role === 'admin' || me.role === 'reseller';
  const ownerQ = owner ? `?owner=${encodeURIComponent(owner)}` : '';

  const load = useCallback(async (o = owner) => {
    const qs = o ? `?owner=${encodeURIComponent(o)}` : '';
    try {
      const res = await api.get(`/backups${qs}`);
      setRows(res.backups || []);
      setLimit(res.max_upload_bytes || 0);
    } catch (err) {
      msg.fail(err);
    }
    try {
      const res = await api.get(`/backups/schedule${qs}`);
      setSchedule(res.schedule);
    } catch { /* an account with no schedule is not an error */ }
  }, [owner]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (staff) {
      api.get('/users').then((r) => {
        const list = (r.users || []).filter((u) => u.linux_uid);
        setOwners(list);
        if (list.length) { setOwner(list[0].username); load(list[0].username); }
        else load('');
      }).catch(() => load(''));
    } else {
      load('');
    }
  }, [staff]); // eslint-disable-line react-hooks/exhaustive-deps

  // A running backup finishes on the server, so poll while one is in flight
  // and stop as soon as none are.
  useEffect(() => {
    if (!rows.some((b) => b.status === 'running')) return undefined;
    const t = setTimeout(() => load(), 5000);
    return () => clearTimeout(t);
  }, [rows, load]);

  async function guard(fn, okText) {
    msg.clear();
    try {
      await fn();
      msg.ok(okText);
    } catch (err) {
      msg.fail(err);
    }
    await load();
  }

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Destinations me={me} />

      <Card>
        <div className="toolbar">
          {staff && (
            <div style={{ flex: '0 0 12rem' }}>
              <label htmlFor="bkOwner">Account</label>
              <select
                id="bkOwner"
                value={owner}
                onChange={(e) => { setOwner(e.target.value); load(e.target.value); }}
              >
                {owners.length === 0 && <option value="">no hosting accounts yet</option>}
                {owners.map((u) => <option key={u.id}>{u.username}</option>)}
              </select>
            </div>
          )}
          <div>
            <label>Include</label>
            <label style={{ display: 'inline', color: 'var(--ink)' }}>
              <input
                type="checkbox"
                checked={includeFiles}
                onChange={(e) => setIncludeFiles(e.target.checked)}
              />{' '}
              files
            </label>
            <label style={{ display: 'inline', color: 'var(--ink)', marginLeft: '.8rem' }}>
              <input
                type="checkbox"
                checked={includeDatabases}
                onChange={(e) => setIncludeDatabases(e.target.checked)}
              />{' '}
              databases
            </label>
          </div>
          <div>
            <button
              type="button"
              className="primary"
              onClick={() => guard(
                () => api.post(`/backups${ownerQ}`, {
                  include_files: includeFiles, include_databases: includeDatabases,
                }),
                'Backup started',
              )}
            >
              Back up now
            </button>{' '}
            <button type="button" onClick={() => load()}>Refresh</button>
          </div>
        </div>
      </Card>

      <ScheduleCard schedule={schedule} ownerQ={ownerQ} onSaved={load} msg={msg} />

      <Card title="Archives">
        {rows.length === 0 ? (
          <Empty>No backups yet.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>Taken</th><th>Account</th><th>Kind</th><th>Contents</th>
                  <th>Size</th><th>Status</th><th />
                </tr>
              </thead>
              <tbody>
                {rows.map((b) => (
                  <tr key={b.id}>
                    <td>
                      {fmtDate(b.created_at)}
                      <div className="muted" style={{ fontSize: '.8em' }}>{b.filename}</div>
                    </td>
                    <td>{b.owner}</td>
                    <td className="muted">{b.kind}</td>
                    <td className="muted">
                      {[b.has_files ? 'files' : null,
                        (b.databases || []).length
                          ? `${b.databases.length} database${b.databases.length === 1 ? '' : 's'}`
                          : null].filter(Boolean).join(' + ') || '—'}
                      {b.file_count > 0 && <div>{b.file_count} files</div>}
                    </td>
                    <td className="muted">{b.size_bytes ? fmtBytes(b.size_bytes) : '—'}</td>
                    <td>
                      {b.status === 'ready' && <Tag kind="ok">ready</Tag>}
                      {b.status === 'running' && <Tag kind="warn">running…</Tag>}
                      {b.status === 'failed' && <Tag kind="bad">failed</Tag>}
                      {b.error && (
                        <div className="muted" style={{ fontSize: '.8em' }}>{b.error}</div>
                      )}
                    </td>
                    <td className="right nowrap">
                      {b.status === 'ready' && (
                        <>
                          <button
                            type="button"
                            className="link"
                            onClick={() => download(`/backups/${b.id}/download`)}
                          >
                            Download
                          </button>{' '}
                          <button
                            type="button"
                            className="link"
                            onClick={async () => {
                              const ok = await ask({
                                title: `Restore ${b.filename} over ${b.owner}?`,
                                body:
                                  'Files in the archive overwrite the ones on the server, and every '
                                  + 'database in it is replaced — tables created since this backup '
                                  + 'was taken will be gone.\n\nAnything the account has changed '
                                  + 'since then is lost. This cannot be undone.',
                                confirmLabel: 'Restore',
                                danger: true,
                              });
                              if (!ok) return;
                              msg.ok('Restoring. This can take several minutes — leave the page open.');
                              try {
                                const res = await api.post(`/backups/${b.id}/restore`, {});
                                msg.ok(
                                  `Restored ${res.restored_files} files`
                                  + ((res.restored_databases || []).length
                                    ? ` and ${res.restored_databases.length} database(s)` : ''),
                                );
                              } catch (err) {
                                msg.fail(err);
                              }
                              await load();
                            }}
                          >
                            Restore
                          </button>{' '}
                        </>
                      )}
                      {b.status !== 'running' && (
                        <button
                          type="button"
                          className="link"
                          onClick={async () => {
                            const ok = await ask({
                              title: `Delete ${b.filename}?`,
                              body: 'The archive is removed from the server for good.',
                              confirmLabel: 'Delete',
                              danger: true,
                            });
                            if (ok) guard(() => api.del(`/backups/${b.id}`), 'Backup deleted');
                          }}
                        >
                          Delete
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <form
          style={{ marginTop: '.9rem' }}
          onSubmit={async (e) => {
            e.preventDefault();
            const f = archive.current && archive.current.files[0];
            if (!f) { msg.fail(new Error('Choose an archive first')); return; }
            if (limit && f.size > limit) {
              msg.fail(new Error(`${f.name} is ${fmtBytes(f.size)}, over the ${fmtBytes(limit)} limit.`));
              return;
            }
            const body = new FormData();
            body.append('file', f, f.name);
            msg.ok(`Uploading ${f.name}…`);
            try {
              await api.post(`/backups/upload${ownerQ}`, body);
              msg.ok('Archive uploaded');
              archive.current.value = '';
            } catch (err) {
              msg.fail(err);
            }
            await load();
          }}
        >
          <div style={{
            border: '1px dashed var(--line)', borderRadius: 'var(--radius)',
            padding: '.9rem', textAlign: 'center',
          }}
          >
            <input type="file" accept=".gz,.tgz" ref={archive} style={{ width: 'auto' }} />{' '}
            <button type="submit">Upload an archive</button>
            <div className="muted" style={{ marginTop: '.4rem', fontSize: '.85rem' }}>
              Brings a .tar.gz written by this panel back onto the server, ready to restore.
            </div>
          </div>
        </form>
      </Card>
    </>
  );
}

function ScheduleCard({ schedule, ownerQ, onSaved, msg }) {
  const [form, setForm] = useState({
    enabled: false, frequency: 'daily', hour: 3, weekday: 0, keep: 7,
  });

  useEffect(() => {
    if (schedule) {
      setForm({
        enabled: schedule.enabled, frequency: schedule.frequency,
        hour: schedule.hour, weekday: schedule.weekday, keep: schedule.keep,
      });
    } else {
      setForm({ enabled: false, frequency: 'daily', hour: 3, weekday: 0, keep: 7 });
    }
  }, [schedule]);

  const set = (k) => (e) => setForm({
    ...form,
    [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value,
  });

  return (
    <Card title="Automatic backups">
      <form
        className="row"
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await api.put(`/backups/schedule${ownerQ}`, {
              enabled: form.enabled === true || form.enabled === 'true',
              frequency: form.frequency,
              hour: Number(form.hour),
              weekday: Number(form.weekday),
              keep: Number(form.keep) || 7,
              // A schedule always takes everything: an automatic backup that
              // quietly skipped the databases is a trap.
              include_files: true,
              include_databases: true,
            });
            msg.ok('Schedule saved');
          } catch (err) {
            msg.fail(err);
          }
          onSaved();
        }}
      >
        <div className="field" style={{ flex: '0 0 8rem' }}>
          <label htmlFor="bsEnabled">Status</label>
          <select
            id="bsEnabled"
            value={form.enabled ? '1' : '0'}
            onChange={(e) => setForm({ ...form, enabled: e.target.value === '1' })}
          >
            <option value="0">off</option>
            <option value="1">on</option>
          </select>
        </div>
        <div className="field" style={{ flex: '0 0 8rem' }}>
          <label htmlFor="bsFreq">How often</label>
          <select id="bsFreq" value={form.frequency} onChange={set('frequency')}>
            <option value="daily">daily</option>
            <option value="weekly">weekly</option>
          </select>
        </div>
        {form.frequency === 'weekly' && (
          <div className="field" style={{ flex: '0 0 9rem' }}>
            <label htmlFor="bsDay">Day</label>
            <select id="bsDay" value={form.weekday} onChange={set('weekday')}>
              {['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']
                .map((d, i) => <option key={d} value={i}>{d}</option>)}
            </select>
          </div>
        )}
        <div className="field" style={{ flex: '0 0 7rem' }}>
          <label htmlFor="bsHour">Hour</label>
          <select id="bsHour" value={form.hour} onChange={set('hour')}>
            {Array.from({ length: 24 }, (_, h) => (
              <option key={h} value={h}>{String(h).padStart(2, '0')}:00</option>
            ))}
          </select>
        </div>
        <div className="field" style={{ flex: '0 0 6rem' }}>
          <label htmlFor="bsKeep">Keep</label>
          <input id="bsKeep" type="number" min="1" max="60" value={form.keep} onChange={set('keep')} />
        </div>
        <div>
          <button type="submit" className="primary">Save schedule</button>{' '}
          <button
            type="button"
            onClick={async () => {
              try {
                await api.del(`/backups/schedule${ownerQ}`);
                msg.ok('Automatic backups turned off');
              } catch (err) {
                msg.fail(err);
              }
              onSaved();
            }}
          >
            Turn off
          </button>
        </div>
      </form>
      <p className="muted" style={{ margin: '.5rem 0 0', fontSize: '.83rem' }}>
        {!schedule
          ? 'No automatic backups for this account yet.'
          : schedule.last_run_at
            ? `Last run ${fmtDate(schedule.last_run_at)}${schedule.last_error ? ` — ${schedule.last_error}` : ''}`
            : 'Never run yet.'}
        {' '}Retention only removes automatic backups, never one you took by hand.
      </p>
    </Card>
  );
}
