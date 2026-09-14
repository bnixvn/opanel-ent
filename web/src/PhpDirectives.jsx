import React from 'react';
import { Hint } from './components.jsx';

// PhpDirectives renders a php.ini form from the catalogue the API returns.
//
// Drawn from the server's list rather than one kept here, so a directive
// added or withdrawn appears or disappears without a frontend change — and
// nobody is shown a field the server would refuse to save.
export default function PhpDirectives({ directives, values, onChange }) {
  return (
    <div className="grid2">
      {directives.map((d) => (
        <Field
          key={d.name}
          directive={d}
          value={values[d.name] ?? ''}
          onChange={(v) => onChange({ ...values, [d.name]: v })}
        />
      ))}
    </div>
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
    // A checkbox each: a comma-separated text field over a fixed set is a
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
        <Hint>
          {d.name}
          {d.help ? ` — ${d.help}` : ''}
        </Hint>
      </label>
      {input}
    </div>
  );
}
