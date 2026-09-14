import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Message, useMessage } from '../components.jsx';

// PhpSettings edits one website's php.ini overrides.
//
// The form is drawn from the catalogue the API returns rather than from a
// list kept here, so a directive added or withdrawn on the server appears or
// disappears without a frontend change -- and a customer never sees a field
// they are not allowed to submit.
export default function PhpSettings({ site, onClose }) {
  const [data, setData] = useState(null);
  const [values, setValues] = useState({});
  const [saving, setSaving] = useState(false);
  const msg = useMessage();

  const load = useCallback(async () => {
    try {
      const res = await api.get(`/sites/${site.id}/php`);
      setData(res);
      setValues(res.values || {});
    } catch (err) {
      msg.fail(err);
    }
  }, [site.id]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  function set(name, value) {
    setValues((v) => ({ ...v, [name]: value }));
  }

  async function save() {
    msg.clear();
    setSaving(true);
    try {
      await api.put(`/sites/${site.id}/php`, { values });
      msg.ok('Saved. The webserver has been reloaded.');
      await load();
    } catch (err) {
      msg.fail(err);
    }
    setSaving(false);
  }

  const changed = data
    && JSON.stringify(values) !== JSON.stringify(data.values || {});

  return (
    <Card
      title={`PHP settings — ${site.domain}`}
      actions={<button type="button" onClick={onClose}>Close</button>}
    >
      <Message value={msg.message} onClear={msg.clear} />

      {!data ? (
        <p className="muted">Loading…</p>
      ) : (
        <>
          <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
            These apply to <code>{site.domain}</code> only, on PHP {data.php}.
            Leave a field empty to use PHP&apos;s own default.
          </p>

          <div className="grid2">
            {data.directives.map((d) => (
              <Field
                key={d.name}
                directive={d}
                value={values[d.name] ?? ''}
                onChange={(v) => set(d.name, v)}
              />
            ))}
          </div>

          <div className="row" style={{ marginTop: '.8rem', alignItems: 'center' }}>
            <div>
              <button type="button" className="primary" disabled={saving || !changed} onClick={save}>
                {saving ? 'Saving…' : 'Save and reload'}
              </button>{' '}
              <button
                type="button"
                disabled={saving || Object.keys(values).length === 0}
                onClick={() => setValues({})}
              >
                Clear all
              </button>
            </div>
            <p className="muted" style={{ flex: 1, margin: 0, fontSize: '.8rem' }}>
              Saving rewrites this website&apos;s webserver configuration and reloads
              the server. Other websites are not affected.
            </p>
          </div>
        </>
      )}
    </Card>
  );
}

function Field({ directive: d, value, onChange }) {
  const id = `php-${d.name}`;
  let input;

  if (d.kind === 'bool') {
    input = (
      <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">default ({d.default})</option>
        <option value="On">On</option>
        <option value="Off">Off</option>
      </select>
    );
  } else if (d.kind === 'enum') {
    input = (
      <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">default ({d.default})</option>
        {(d.options || []).map((o) => <option key={o} value={o}>{o}</option>)}
      </select>
    );
  } else if (d.kind === 'list') {
    // A checkbox each: a comma-separated text field for a fixed set is a
    // typo waiting to be rejected by the server.
    const current = value === '' ? (d.default || '').split(',') : value.split(',');
    const chosen = new Set(current.filter(Boolean));
    input = (
      <div className="checks">
        {(d.options || []).map((o) => (
          <label key={o} className="check">
            <input
              type="checkbox"
              checked={chosen.has(o)}
              onChange={(e) => {
                const next = new Set(chosen);
                if (e.target.checked) next.add(o); else next.delete(o);
                onChange([...next].sort().join(','));
              }}
            />
            <code>{o}</code>
          </label>
        ))}
      </div>
    );
  } else {
    input = (
      <input
        id={id}
        value={value}
        placeholder={d.default}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  return (
    <div className="field">
      <label htmlFor={id}>
        {d.label}
        {d.staff_only && <> <span className="muted" style={{ fontWeight: 400 }}>(staff)</span></>}
      </label>
      {input}
      <div className="muted" style={{ fontSize: '.78rem' }}>
        <code>{d.name}</code>{d.help ? ` — ${d.help}` : ''}
      </div>
    </div>
  );
}
