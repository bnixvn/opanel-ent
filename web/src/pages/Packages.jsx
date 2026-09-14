import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, useConfirm, useMessage } from '../components.jsx';

export default function Packages() {
  const [plans, setPlans] = useState([]);
  const [draft, setDraft] = useState({ name: '', max_sites: 1, max_databases: 1, disk_quota_mb: 1024 });
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(async () => {
    try {
      setPlans((await api.get('/plans')).plans || []);
    } catch (err) {
      msg.fail(err);
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

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

  const num = (k) => (e) => setDraft({ ...draft, [k]: Number(e.target.value) || 0 });

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card title="New package">
        <form
          className="row"
          onSubmit={(e) => {
            e.preventDefault();
            guard(() => api.post('/plans', { ...draft, name: draft.name.trim() }), 'Package created');
            setDraft({ ...draft, name: '' });
          }}
        >
          <div className="field">
            <label htmlFor="npName">Name</label>
            <input
              id="npName"
              placeholder="Starter"
              required
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            />
          </div>
          <div className="field" style={{ flex: '0 0 8rem' }}>
            <label htmlFor="npSites">Max websites</label>
            <input id="npSites" type="number" min="0" value={draft.max_sites} onChange={num('max_sites')} />
          </div>
          <div className="field" style={{ flex: '0 0 8rem' }}>
            <label htmlFor="npDbs">Max databases</label>
            <input id="npDbs" type="number" min="0" value={draft.max_databases} onChange={num('max_databases')} />
          </div>
          <div className="field" style={{ flex: '0 0 8rem' }}>
            <label htmlFor="npDisk">Disk (MB)</label>
            <input id="npDisk" type="number" min="0" value={draft.disk_quota_mb} onChange={num('disk_quota_mb')} />
          </div>
          <div><button type="submit" className="primary">Create package</button></div>
        </form>
      </Card>

      <Card title="Packages">
        {plans.length === 0 ? (
          <Empty>No packages yet. Accounts without one are unlimited.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>Package</th><th>Websites</th><th>Databases</th>
                  <th>Disk</th><th>Accounts</th><th />
                </tr>
              </thead>
              <tbody>
                {plans.map((p) => <PlanRow key={p.id} plan={p} guard={guard} ask={ask} />)}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}

function PlanRow({ plan, guard, ask }) {
  const [form, setForm] = useState(plan);
  useEffect(() => setForm(plan), [plan]);
  const num = (k) => (e) => setForm({ ...form, [k]: Number(e.target.value) || 0 });

  return (
    <tr>
      <td>
        <strong>{plan.name}</strong>
        {plan.description && (
          <div className="muted" style={{ fontSize: '.85em' }}>{plan.description}</div>
        )}
      </td>
      <td><input type="number" min="0" style={{ width: '5.5rem' }} value={form.max_sites} onChange={num('max_sites')} /></td>
      <td><input type="number" min="0" style={{ width: '5.5rem' }} value={form.max_databases} onChange={num('max_databases')} /></td>
      <td>
        <input type="number" min="0" style={{ width: '6.5rem' }} value={form.disk_quota_mb} onChange={num('disk_quota_mb')} />
        <span className="muted"> MB</span>
      </td>
      <td className="muted">{plan.users}</td>
      <td className="right nowrap">
        <button
          type="button"
          onClick={() => guard(
            () => api.patch(`/plans/${plan.id}`, {
              name: form.name,
              description: form.description || '',
              max_sites: form.max_sites,
              max_databases: form.max_databases,
              disk_quota_mb: form.disk_quota_mb,
              default_php: form.default_php || '',
            }),
            `${plan.name} saved`,
          )}
        >
          Save
        </button>{' '}
        <button
          type="button"
          className="danger"
          onClick={async () => {
            const ok = await ask({
              title: `Delete ${plan.name}?`,
              body: plan.users > 0
                ? `${plan.users} account(s) are on it and will be left unlimited `
                  + 'until you assign another package.'
                : 'Nothing is using it.',
              confirmLabel: 'Delete',
              danger: true,
            });
            if (ok) guard(() => api.del(`/plans/${plan.id}`), 'Package deleted');
          }}
        >
          Delete
        </button>
      </td>
    </tr>
  );
}
