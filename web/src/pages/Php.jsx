import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

export default function Php() {
  const [data, setData] = useState({ provider: '', versions: [] });
  const [busy, setBusy] = useState(null);
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
                <tr><th>Version</th><th>Status</th><th>Interpreter</th><th /></tr>
              </thead>
              <tbody>
                {data.versions.map((v) => (
                  <tr key={v.version}>
                    <td><strong>{v.version}</strong></td>
                    <td>
                      {v.installed ? <Tag kind="ok">installed</Tag> : <Tag>available</Tag>}
                      {v.is_default && <> <Tag kind="mute">default</Tag></>}
                    </td>
                    <td className="muted">
                      {v.installed ? <code>{v.cli_path || v.lsapi_path}</code> : '—'}
                    </td>
                    <td className="right nowrap">
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
        <p className="muted" style={{ fontSize: '.83rem', marginBottom: 0 }}>
          Each website chooses its own version on the Websites page. Installing
          one here makes it available to choose.
        </p>
      </Card>
    </>
  );
}
