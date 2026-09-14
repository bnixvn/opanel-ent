import React, { useCallback, useEffect, useState } from 'react';
import { api, fmtDate } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

export default function Cron({ me }) {
  const [data, setData] = useState(null);
  const [owners, setOwners] = useState([]);
  const [owner, setOwner] = useState('');
  const [editing, setEditing] = useState(null);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const staff = me.role === 'admin' || me.role === 'reseller';
  const q = owner ? `?owner=${encodeURIComponent(owner)}` : '';

  const load = useCallback(async (who = owner) => {
    const params = who ? `?owner=${encodeURIComponent(who)}` : '';
    try {
      setData(await api.get(`/cron${params}`));
    } catch (err) {
      msg.fail(err);
      setData({ jobs: [] });
    }
  }, [owner]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (staff) {
      api.get('/users').then((r) => {
        const list = (r.users || []).filter((u) => u.linux_uid);
        setOwners(list);
        if (list.length) {
          setOwner(list[0].username);
          load(list[0].username);
        } else {
          setData({ jobs: [] });
        }
      }).catch(() => load(''));
    } else {
      load('');
    }
  }, [staff]); // eslint-disable-line react-hooks/exhaustive-deps

  async function guard(fn, okText) {
    msg.clear();
    setBusy(true);
    try {
      await fn();
      msg.ok(okText);
      setEditing(null);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    await load();
  }

  if (!data) return <Message value={msg.message} onClear={msg.clear} />;

  const jobs = data.jobs || [];

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      {staff && (
        <Card>
          <div className="row">
            <div className="field" style={{ flex: '0 0 14rem' }}>
              <label htmlFor="cronOwner">Account</label>
              <select
                id="cronOwner"
                value={owner}
                onChange={(e) => { setOwner(e.target.value); setEditing(null); load(e.target.value); }}
              >
                {owners.length === 0 && <option value="">no hosting accounts yet</option>}
                {owners.map((u) => <option key={u.id}>{u.username}</option>)}
              </select>
            </div>
            <p className="muted" style={{ flex: 1, alignSelf: 'center', margin: 0, fontSize: '.83rem' }}>
              Jobs run as this Linux account, with its own permissions and its
              own disk quota.
            </p>
          </div>
        </Card>
      )}

      {data.allowed === false && (
        <p className="msg err">
          This account is listed in the server&apos;s cron.deny, so jobs saved
          here will never run. An administrator has to allow it first.
        </p>
      )}

      <Card title={editing && editing.id ? 'Edit job' : 'New job'}>
        <JobForm
          key={editing ? editing.id || 'new' : 'new'}
          job={editing}
          presets={data.presets || []}
          busy={busy}
          onCancel={() => setEditing(null)}
          onSubmit={(body) => guard(
            () => (editing && editing.id
              ? api.patch(`/cron/${editing.id}`, body)
              : api.post(`/cron${q}`, body)),
            editing && editing.id ? 'Job saved' : 'Job added',
          )}
        />
      </Card>

      <Card title="Scheduled jobs">
        {jobs.length === 0 ? (
          <Empty>Nothing is scheduled for this account.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr><th>When</th><th>Command</th><th>Status</th><th /></tr>
              </thead>
              <tbody>
                {jobs.map((j) => (
                  <tr key={j.id}>
                    <td className="nowrap"><code>{j.schedule}</code></td>
                    <td style={{ wordBreak: 'break-all', fontSize: '.83rem' }}>
                      <code>{j.command}</code>
                      {j.comment && (
                        <div className="muted" style={{ fontSize: '.78rem' }}>{j.comment}</div>
                      )}
                    </td>
                    <td>
                      {j.enabled ? <Tag kind="ok">on</Tag> : <Tag kind="mute">paused</Tag>}
                    </td>
                    <td className="right nowrap">
                      <button
                        type="button"
                        className="link"
                        disabled={busy}
                        onClick={() => guard(
                          () => api.patch(`/cron/${j.id}`, { enabled: !j.enabled }),
                          j.enabled ? 'Job paused' : 'Job resumed',
                        )}
                      >
                        {j.enabled ? 'Pause' : 'Resume'}
                      </button>{' '}
                      <button type="button" className="link" disabled={busy}
                        onClick={() => setEditing(j)}
                      >
                        Edit
                      </button>{' '}
                      <button
                        type="button"
                        className="link"
                        disabled={busy}
                        onClick={async () => {
                          const ok = await ask({
                            title: 'Delete this job?',
                            body: j.command,
                            confirmLabel: 'Delete',
                            danger: true,
                          });
                          if (ok) guard(() => api.del(`/cron/${j.id}`), 'Job deleted');
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
        <p className="muted" style={{ fontSize: '.82rem', marginBottom: 0 }}>
          Output is not emailed — this server has no mail transport, and cron
          that cannot deliver logs a failure on every run. Redirect what you
          want to keep, for example{' '}
          <code>{'>> $HOME/logs/cron.log 2>&1'}</code>.
          {jobs.length > 0 && data.max && <> Up to {data.max} jobs per account.</>}
        </p>
      </Card>
    </>
  );
}

function JobForm({ job, presets, busy, onSubmit, onCancel }) {
  const [schedule, setSchedule] = useState(job ? job.schedule : '0 3 * * *');
  const [command, setCommand] = useState(job ? job.command : '');
  const [comment, setComment] = useState(job ? job.comment : '');

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit({ schedule: schedule.trim(), command: command.trim(), comment: comment.trim() });
      }}
    >
      <div className="row">
        <div className="field" style={{ flex: '0 0 12rem' }}>
          <label htmlFor="cronPreset">How often</label>
          <select
            id="cronPreset"
            value={presets.some((p) => p.schedule === schedule) ? schedule : ''}
            onChange={(e) => e.target.value && setSchedule(e.target.value)}
          >
            <option value="">custom…</option>
            {presets.map((p) => (
              <option key={p.schedule} value={p.schedule}>{p.label}</option>
            ))}
          </select>
        </div>
        <div className="field" style={{ flex: '0 0 10rem' }}>
          <label htmlFor="cronSpec">Schedule</label>
          <input
            id="cronSpec"
            required
            value={schedule}
            onChange={(e) => setSchedule(e.target.value)}
            style={{ fontFamily: 'ui-monospace, monospace' }}
          />
          <div className="muted" style={{ fontSize: '.75rem' }}>
            minute hour day month weekday
          </div>
        </div>
        <div className="field">
          <label htmlFor="cronNote">Note</label>
          <input
            id="cronNote"
            placeholder="nightly backup"
            value={comment}
            onChange={(e) => setComment(e.target.value)}
          />
        </div>
      </div>
      <div className="field" style={{ marginTop: '.5rem' }}>
        <label htmlFor="cronCmd">Command</label>
        <input
          id="cronCmd"
          required
          placeholder="/usr/bin/php $HOME/example.com/public_html/cron.php"
          value={command}
          onChange={(e) => setCommand(e.target.value)}
          style={{ fontFamily: 'ui-monospace, monospace' }}
        />
      </div>
      <p style={{ marginBottom: 0 }}>
        <button type="submit" className="primary" disabled={busy}>
          {busy ? 'Saving…' : job && job.id ? 'Save job' : 'Add job'}
        </button>{' '}
        {job && job.id && (
          <button type="button" onClick={onCancel} disabled={busy}>Cancel</button>
        )}
        {job && job.id && job.updated_at && (
          <span className="muted" style={{ marginLeft: '.6rem', fontSize: '.8rem' }}>
            last changed {fmtDate(job.updated_at)}
          </span>
        )}
      </p>
    </form>
  );
}
