import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';
import PhpDirectives from '../PhpDirectives.jsx';

export default function Php({ me }) {
  const [data, setData] = useState({ provider: '', versions: [] });
  const [busy, setBusy] = useState(null);
  const [tuning, setTuning] = useState(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(async () => {
    try {
      setData(await api.get('/php/versions'));
    } catch (err) {
      msg.fail(err);
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  async function run(version, fn, okText) {
    msg.clear();
    setBusy(version);
    try {
      await fn();
      msg.ok(okText);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(null);
    await load();
  }

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card title={`PHP versions (${data.provider || 'unknown provider'})`}>
        {(data.versions || []).length === 0 ? (
          <Empty>No versions reported.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr><th>Version</th><th>Status</th><th>ionCube</th><th>Interpreter</th><th /></tr>
              </thead>
              <tbody>
                {data.versions.map((v) => (
                  <tr key={v.version}>
                    <td><strong>{v.version}</strong></td>
                    <td>
                      {v.installed ? <Tag kind="ok">installed</Tag> : <Tag>available</Tag>}
                      {v.is_default && <> <Tag kind="mute">default</Tag></>}
                    </td>
                    <td>
                      {!v.installed ? <span className="muted">—</span>
                        : v.ioncube ? <Tag kind="ok">loaded</Tag>
                          : <Tag kind="mute">not installed</Tag>}
                    </td>
                    <td className="muted">
                      {v.installed ? <code>{v.cli_path || v.lsapi_path}</code> : '—'}
                    </td>
                    <td className="right nowrap">
                      {v.installed && (
                        <>
                          <button
                            type="button"
                            className="link"
                            onClick={() => setTuning(tuning === v.version ? null : v.version)}
                          >
                            {tuning === v.version ? 'Hide settings' : 'Settings'}
                          </button>{' '}
                        </>
                      )}
                      {v.installed ? (
                        <button
                          type="button"
                          className="danger"
                          disabled={busy === v.version}
                          onClick={async () => {
                            const ok = await ask({
                              title: `Remove PHP ${v.version}?`,
                              body:
                                'Any website still set to this version would stop working, '
                                + 'so the panel refuses while one is using it.',
                              confirmLabel: 'Remove',
                              danger: true,
                            });
                            if (ok) {
                              run(v.version, () => api.del(`/php/versions/${v.version}`),
                                `PHP ${v.version} removed`);
                            }
                          }}
                        >
                          {busy === v.version ? 'Working…' : 'Remove'}
                        </button>
                      ) : (
                        <button
                          type="button"
                          disabled={busy === v.version}
                          onClick={() => run(
                            v.version,
                            () => api.post(`/php/versions/${v.version}/install`, {}),
                            `PHP ${v.version} installed`,
                          )}
                        >
                          {busy === v.version ? 'Installing…' : 'Install'}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {tuning && (
        <VersionSettings
          version={tuning}
          canEdit={me.role === 'admin'}
          onClose={() => setTuning(null)}
        />
      )}
    </>
  );
}

// VersionSettings edits the php.ini every website on one version shares.
//
// Per version rather than per website, because that is the shape of the
// decision: a memory limit or an upload size is a property of the hosting,
// and setting it once is one decision instead of one per site.
function VersionSettings({ version, canEdit, onClose }) {
  const [data, setData] = useState(null);
  const [values, setValues] = useState({});
  const [saving, setSaving] = useState(false);
  const msg = useMessage();

  const load = useCallback(async () => {
    try {
      const res = await api.get(`/php/versions/${version}/settings`);
      setData(res);
      setValues(res.values || {});
    } catch (err) {
      msg.fail(err);
    }
  }, [version]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  const changed = data && JSON.stringify(values) !== JSON.stringify(data.values || {});

  return (
    <Card
      title={`PHP ${version} settings`}
      actions={<button type="button" onClick={onClose}>Close</button>}
    >
      <Message value={msg.message} onClear={msg.clear} />

      {!data ? <p className="muted">Loading…</p> : (
        <>

          <PhpDirectives
            directives={data.directives || []}
            values={values}
            onChange={setValues}
          />

          <Overrides version={version} canEdit={canEdit} msg={msg} />

          <div className="row" style={{ marginTop: '.8rem', alignItems: 'center' }}>
            <div>
              <button
                type="button"
                className="primary"
                disabled={!canEdit || saving || !changed}
                onClick={async () => {
                  msg.clear();
                  setSaving(true);
                  try {
                    const res = await api.put(`/php/versions/${version}/settings`, { values });
                    if (res.warning) msg.warn(res.warning);
                    else msg.ok(`Saved. Every website on PHP ${version} is using these now.`);
                    await load();
                  } catch (err) {
                    msg.fail(err);
                  }
                  setSaving(false);
                }}
              >
                {saving ? 'Saving…' : 'Save and reload'}
              </button>{' '}
              <button
                type="button"
                disabled={!canEdit || saving || Object.keys(values).length === 0}
                onClick={() => setValues({})}
              >
                Clear all
              </button>
            </div>
          </div>
        </>
      )}
    </Card>
  );
}


// Overrides names the websites that are not following the version settings.
//
// A setting nobody can see still applies. Without this list an operator
// raises a limit here, one website does not change, and there is no screen
// anywhere that explains why — which is the worst way to spend an afternoon.
function Overrides({ version, canEdit, msg }) {
  const [rows, setRows] = useState(null);

  const load = useCallback(async () => {
    try {
      const res = await api.get('/php/overrides');
      setRows((res.overrides || []).filter((o) => o.php === version));
    } catch { /* the settings above still work */ }
  }, [version]);

  useEffect(() => { load(); }, [load]);

  if (!rows || rows.length === 0) return null;

  return (
    <div style={{ marginTop: '1rem' }}>
      <p style={{ margin: '0 0 .3rem', fontWeight: 600, fontSize: '.9rem' }}>
        Not following these settings
      </p>
      <div className="scroll">
        <table>
          <thead><tr><th>Website</th><th>Overrides</th><th /></tr></thead>
          <tbody>
            {rows.map((o) => (
              <tr key={o.site_id}>
                <td><code>{o.domain}</code></td>
                <td className="muted" style={{ fontSize: '.82rem' }}>
                  {Object.entries(o.values)
                    .map(([k, v]) => `${k}=${v}`).join(', ')}
                </td>
                <td className="right">
                  <button
                    type="button"
                    className="link"
                    disabled={!canEdit}
                    onClick={async () => {
                      msg.clear();
                      try {
                        await api.del(`/sites/${o.site_id}/php`);
                        msg.ok(`${o.domain} now follows the PHP ${version} settings.`);
                        await load();
                      } catch (err) {
                        msg.fail(err);
                      }
                    }}
                  >
                    Use the version settings
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
