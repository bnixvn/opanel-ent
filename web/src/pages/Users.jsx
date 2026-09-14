import React, { useCallback, useEffect, useState } from 'react';
import { api, fmtBytes } from '../api.js';
import { Card, Empty, Message, Search, Secret, Tag, matches, useConfirm, useMessage } from '../components.jsx';

export default function Users({ me, onImpersonated }) {
  const [users, setUsers] = useState([]);
  const [plans, setPlans] = useState([]);
  const [usage, setUsage] = useState({});
  const [query, setQuery] = useState('');
  const [created, setCreated] = useState(null);
  const [limitsFor, setLimitsFor] = useState(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const isAdmin = me.role === 'admin';

  const load = useCallback(async () => {
    try {
      const res = await api.get('/users');
      setUsers(res.users || []);
      (res.users || []).filter((u) => u.linux_uid).forEach((u) => {
        api.get(`/users/${u.id}/usage`)
          .then((r) => setUsage((prev) => ({ ...prev, [u.id]: r })))
          .catch(() => {});
      });
    } catch (err) {
      msg.fail(err);
    }
    try {
      setPlans((await api.get('/plans')).plans || []);
    } catch { /* a panel with no packages still lists users */ }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  async function guard(fn, okText) {
    msg.clear();
    try {
      const res = await fn();
      msg.ok(okText);
      return res;
    } catch (err) {
      msg.fail(err);
      return null;
    } finally {
      load();
    }
  }

  const shown = users.filter((u) => matches(u.username, query) || matches(u.email, query));

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <NewUser isAdmin={isAdmin} plans={plans} onDone={guard} onCreated={setCreated} />

      {created && (
        <Card title={`Account ${created.user.username} created`}>
          <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
            These are shown once.
          </p>
          <Secret label="Panel password" value={created.password} />
          <Secret label="SFTP password" value={created.sftp_password} />
          <button type="button" onClick={() => setCreated(null)}>Close</button>
        </Card>
      )}

      <Card
        title="Accounts"
        actions={<Search value={query} onChange={setQuery} placeholder="Search accounts…" />}
      >
        {shown.length === 0 ? (
          <Empty>{users.length ? 'Nothing matches that search.' : 'No accounts yet.'}</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>User</th><th>Role</th><th>Package</th><th>Usage</th>
                  <th>2FA</th><th>Status</th><th />
                </tr>
              </thead>
              <tbody>
                {shown.map((u) => {
                  const g = usage[u.id] && (usage[u.id].usage || usage[u.id]);
                  const over = usage[u.id] && usage[u.id].over_quota;
                  return (
                    <tr key={u.id}>
                      <td>
                        <strong>{u.username}</strong>
                        {u.email && <div className="muted" style={{ fontSize: '.85em' }}>{u.email}</div>}
                      </td>
                      <td>
                        {isAdmin && u.id !== me.id ? (
                          <select
                            value={u.role}
                            onChange={(e) => guard(
                              () => api.patch(`/users/${u.id}`, { role: e.target.value }),
                              `${u.username} is now ${e.target.value}`,
                            )}
                          >
                            <option value="admin">admin</option>
                            <option value="reseller">reseller</option>
                            <option value="end_user">end_user</option>
                          </select>
                        ) : (
                          <span className="muted">{u.role}</span>
                        )}
                      </td>
                      <td>
                        {u.linux_uid ? (
                          <select
                            value={(g && g.plan_id) || ''}
                            onChange={(e) => guard(
                              () => api.post(`/users/${u.id}/plan`, {
                                plan_id: e.target.value ? Number(e.target.value) : null,
                              }),
                              e.target.value ? 'Package assigned' : 'Package cleared',
                            )}
                          >
                            <option value="">none</option>
                            {plans.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                          </select>
                        ) : (
                          <span className="muted">—</span>
                        )}
                      </td>
                      <td className="muted">
                        {g ? (
                          <>
                            {g.sites}{g.max_sites ? `/${g.max_sites}` : ''} site,{' '}
                            {g.databases}{g.max_databases ? `/${g.max_databases}` : ''} db
                            <div className={over ? 'tag bad' : undefined}>
                              {fmtBytes(g.disk_bytes)}
                              {g.disk_quota_mb ? ` / ${g.disk_quota_mb} MB` : ''}
                            </div>
                          </>
                        ) : '—'}
                      </td>
                      <td>{u.totp_enabled ? <Tag kind="ok">on</Tag> : <Tag>off</Tag>}</td>
                      <td>{u.suspended ? <Tag kind="warn">suspended</Tag> : <Tag kind="ok">active</Tag>}</td>
                      <td className="right nowrap">
                        {isAdmin && u.role === 'reseller' && (
                          <>
                            <button type="button" className="link" onClick={() => setLimitsFor(u)}>
                              Allowance
                            </button>{' '}
                          </>
                        )}
                        <button
                          type="button"
                          className="link"
                          onClick={async () => {
                            const res = await guard(
                              () => api.post(`/users/${u.id}/password`, {}),
                              'Password reset',
                            );
                            if (res && res.password) {
                              setCreated({ user: u, password: res.password, sftp_password: '' });
                            }
                          }}
                        >
                          Reset password
                        </button>{' '}
                        {u.id !== me.id && u.role === 'end_user' && !u.suspended && (
                          <>
                            <button
                              type="button"
                              className="link"
                              onClick={async () => {
                                const ok = await ask({
                                  title: `Sign in as ${u.username}?`,
                                  body:
                                    'The panel will show exactly what this customer sees, '
                                    + 'and anything you do will be done as them. A banner '
                                    + 'stays on screen until you come back, and both the '
                                    + 'start and the end are recorded in the audit log.',
                                  confirmLabel: 'Sign in as them',
                                });
                                if (!ok) return;
                                try {
                                  const res = await api.post(`/users/${u.id}/impersonate`, {});
                                  onImpersonated(res.user);
                                } catch (err) {
                                  msg.fail(err);
                                }
                              }}
                            >
                              Log in as
                            </button>{' '}
                          </>
                        )}
                        {u.id !== me.id && (
                          <>
                            <button
                              type="button"
                              className="link"
                              onClick={() => guard(
                                () => api.patch(`/users/${u.id}`, { suspended: !u.suspended }),
                                u.suspended ? 'Account unsuspended' : 'Account suspended',
                              )}
                            >
                              {u.suspended ? 'Unsuspend' : 'Suspend'}
                            </button>{' '}
                            <button
                              type="button"
                              className="link"
                              onClick={async () => {
                                const ok = await ask({
                                  title: `Delete ${u.username}?`,
                                  body:
                                    'The Linux account and its home directory go too, '
                                    + 'with every file in it. This cannot be undone.',
                                  confirmLabel: 'Delete',
                                  danger: true,
                                });
                                if (ok) {
                                  guard(
                                    () => api.del(`/users/${u.id}?remove_files=true`),
                                    'Account deleted',
                                  );
                                }
                              }}
                            >
                              Delete
                            </button>
                          </>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {limitsFor && (
        <ResellerLimits
          user={limitsFor}
          onClose={() => setLimitsFor(null)}
          onSaved={(text) => { msg.ok(text); setLimitsFor(null); load(); }}
          onError={msg.fail}
        />
      )}
    </>
  );
}

function NewUser({ isAdmin, plans, onDone, onCreated }) {
  const [form, setForm] = useState({ username: '', email: '', role: 'end_user', plan: '' });
  const set = (k) => (e) => setForm({ ...form, [k]: e.target.value });

  return (
    <Card title="New account">
      <form
        className="row"
        onSubmit={async (e) => {
          e.preventDefault();
          const body = {
            username: form.username.trim(),
            email: form.email.trim() || undefined,
            role: form.role,
          };
          if (form.plan) body.plan_id = Number(form.plan);
          const res = await onDone(() => api.post('/users', body), `Created ${body.username}`);
          if (res) { onCreated(res); setForm({ ...form, username: '', email: '' }); }
        }}
      >
        <div className="field">
          <label htmlFor="nuName">Username</label>
          <input id="nuName" required value={form.username} onChange={set('username')} />
        </div>
        <div className="field">
          <label htmlFor="nuEmail">Email</label>
          <input id="nuEmail" type="email" value={form.email} onChange={set('email')} />
        </div>
        <div className="field" style={{ flex: '0 0 9rem' }}>
          <label htmlFor="nuRole">Role</label>
          <select id="nuRole" value={form.role} onChange={set('role')} disabled={!isAdmin}>
            <option value="end_user">end_user</option>
            {isAdmin && <option value="reseller">reseller</option>}
            {isAdmin && <option value="admin">admin</option>}
          </select>
        </div>
        <div className="field" style={{ flex: '0 0 10rem' }}>
          <label htmlFor="nuPlan">Package</label>
          <select id="nuPlan" value={form.plan} onChange={set('plan')}>
            <option value="">none</option>
            {plans.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
        </div>
        <div><button type="submit" className="primary">Create</button></div>
      </form>
      <p className="muted" style={{ fontSize: '.83rem', margin: '.5rem 0 0' }}>
        An end user gets a Linux account, a home directory and SFTP access.
        Staff accounts do not.
      </p>
    </Card>
  );
}

function ResellerLimits({ user, onClose, onSaved, onError }) {
  const [form, setForm] = useState(null);
  const [used, setUsed] = useState(null);

  useEffect(() => {
    api.get(`/reseller?user=${user.id}`).then((r) => {
      if (r.reseller) { setForm(r.reseller.limits); setUsed(r.reseller.used); }
    }).catch(onError);
  }, [user.id]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!form) return null;
  const num = (k) => (e) => setForm({ ...form, [k]: Number(e.target.value) || 0 });

  return (
    <Card title={`What ${user.username} may hand out`}>
      <form
        className="row"
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await api.put(`/users/${user.id}/reseller-limits`, form);
            onSaved(`Allowance saved for ${user.username}`);
          } catch (err) {
            onError(err);
          }
        }}
      >
        {[
          ['max_accounts', 'Accounts'],
          ['max_sites', 'Websites'],
          ['max_databases', 'Databases'],
          ['disk_quota_mb', 'Disk (MB)'],
          ['bandwidth_gb', 'Bandwidth (GB)'],
        ].map(([key, label]) => (
          <div className="field" key={key} style={{ flex: '0 0 8rem' }}>
            <label htmlFor={key}>{label}</label>
            <input id={key} type="number" min="0" value={form[key]} onChange={num(key)} />
          </div>
        ))}
        <div className="field" style={{ flex: '0 0 12rem' }}>
          <label>Permissions</label>
          <label style={{ display: 'inline', color: 'var(--ink)' }}>
            <input
              type="checkbox"
              checked={form.can_create_plans}
              onChange={(e) => setForm({ ...form, can_create_plans: e.target.checked })}
            />{' '}
            own packages
          </label>
          <label style={{ display: 'block', color: 'var(--ink)' }}>
            <input
              type="checkbox"
              checked={form.allow_oversell}
              onChange={(e) => setForm({ ...form, allow_oversell: e.target.checked })}
            />{' '}
            may oversell
          </label>
        </div>
        <div>
          <button type="submit" className="primary">Save</button>{' '}
          <button type="button" onClick={onClose}>Close</button>
        </div>
      </form>
      {used && (
        <p className="muted" style={{ fontSize: '.83rem', margin: '.6rem 0 0' }}>
          Already handed out: {used.accounts} account(s), {used.sites} website(s),{' '}
          {used.databases} database(s), {used.disk_quota_mb} MB promised. Zero means
          no limit. Disk counts what has been promised, not what is in use.
        </p>
      )}
    </Card>
  );
}
