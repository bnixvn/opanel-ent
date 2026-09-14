import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api, download, fmtBytes, fmtDate } from '../api.js';
import { Card, Empty, Message, useConfirm, useMessage } from '../components.jsx';

export default function Files({ me }) {
  const [owner, setOwner] = useState('');
  const [owners, setOwners] = useState([]);
  const [path, setPath] = useState('');
  const [entries, setEntries] = useState([]);
  const [limits, setLimits] = useState({ max_upload_bytes: 0, max_edit_bytes: 0 });
  const [editing, setEditing] = useState(null);
  const [renaming, setRenaming] = useState(null);
  const [creating, setCreating] = useState(null);
  const fileInput = useRef(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const staff = me.role === 'admin' || me.role === 'reseller';
  const ownerQ = owner ? `owner=${encodeURIComponent(owner)}` : '';
  const q = (extra) => [ownerQ, extra].filter(Boolean).join('&');

  const load = useCallback(async (p = path, o = owner) => {
    const params = [o ? `owner=${encodeURIComponent(o)}` : '', `path=${encodeURIComponent(p)}`]
      .filter(Boolean).join('&');
    try {
      const res = await api.get(`/files?${params}`);
      setEntries(res.entries || []);
      setPath(res.path || '');
      msg.clear();
    } catch (err) {
      msg.fail(err);
      setEntries([]);
    }
  }, [path, owner]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    api.get('/files/limits').then(setLimits).catch(() => {});
    if (staff) {
      api.get('/users').then((r) => {
        const list = (r.users || []).filter((u) => u.linux_uid);
        setOwners(list);
        if (list.length) {
          setOwner(list[0].username);
          load('', list[0].username);
        }
      }).catch(() => {});
    } else {
      load('', '');
    }
  }, [staff]); // eslint-disable-line react-hooks/exhaustive-deps

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

  // Directories first, then files: the order people expect when clicking
  // their way down a tree.
  const sorted = [...entries].sort(
    (a, b) => (b.is_dir - a.is_dir) || a.name.localeCompare(b.name),
  );
  const crumbs = path ? path.split('/') : [];

  async function openEditor(full) {
    try {
      const res = await api.get(`/files/content?${q(`path=${encodeURIComponent(full)}`)}`);
      if (res.truncated) {
        msg.fail(new Error(
          `${full} is ${fmtBytes(res.size)}, larger than the `
          + `${fmtBytes(limits.max_edit_bytes)} the editor can hold. Download it `
          + 'instead — saving here would lose the rest of the file.',
        ));
        return;
      }
      setEditing({ path: full, content: res.content, size: res.size });
    } catch (err) {
      msg.fail(err);
    }
  }

  async function upload(files) {
    const list = [...(files || [])];
    if (!list.length) return;
    const big = list.find((f) => f.size > limits.max_upload_bytes);
    if (big) {
      msg.fail(new Error(
        `${big.name} is ${fmtBytes(big.size)}, over the `
        + `${fmtBytes(limits.max_upload_bytes)} limit. Send it over SFTP instead.`,
      ));
      return;
    }
    const body = new FormData();
    list.forEach((f) => body.append('file', f, f.name));
    msg.ok(`Uploading ${list.length} file(s)…`);
    try {
      await api.post(`/files/upload?${q(`path=${encodeURIComponent(path)}`)}`, body);
      msg.ok(`Uploaded ${list.length} file(s)`);
      if (fileInput.current) fileInput.current.value = '';
    } catch (err) {
      msg.fail(err);
    }
    await load();
  }

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card>
        <div className="toolbar">
          {staff && (
            <div style={{ flex: '0 0 12rem' }}>
              <label htmlFor="fmOwner">Account</label>
              <select
                id="fmOwner"
                value={owner}
                onChange={(e) => { setOwner(e.target.value); setEditing(null); load('', e.target.value); }}
              >
                {owners.length === 0 && <option value="">no hosting accounts yet</option>}
                {owners.map((u) => <option key={u.id}>{u.username}</option>)}
              </select>
            </div>
          )}
          <div className="grow">
            <label>Location</label>
            <div>
              <button type="button" className="link" onClick={() => load('')}>home</button>
              {crumbs.map((c, i) => (
                <span key={`${c}-${i}`}>
                  <span className="muted"> / </span>
                  <button
                    type="button"
                    className="link"
                    onClick={() => load(crumbs.slice(0, i + 1).join('/'))}
                  >
                    {c}
                  </button>
                </span>
              ))}
            </div>
          </div>
          <div className="nowrap">
            <button
              type="button"
              onClick={() => load(path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '')}
              disabled={!path}
            >
              Up
            </button>{' '}
            <button type="button" onClick={() => load()}>Refresh</button>{' '}
            <button type="button" onClick={() => setCreating({ kind: 'dir', name: '' })}>New folder</button>{' '}
            <button type="button" onClick={() => setCreating({ kind: 'file', name: '' })}>New file</button>
          </div>
        </div>

        {creating && (
          <form
            className="row"
            onSubmit={async (e) => {
              e.preventDefault();
              const name = creating.name.trim();
              const full = path ? `${path}/${name}` : name;
              setCreating(null);
              if (creating.kind === 'dir') {
                await guard(() => api.post(`/files/directory?${ownerQ}`, { path: full }), `Created ${name}`);
              } else {
                await guard(
                  () => api.put(`/files/content?${ownerQ}`, { path: full, content: '' }),
                  `Created ${name}`,
                );
                openEditor(full);
              }
            }}
          >
            <div className="field" style={{ flex: '0 1 18rem' }}>
              <label>{creating.kind === 'dir' ? 'Name of the new folder' : 'Name of the new file'}</label>
              <input
                autoFocus
                required
                value={creating.name}
                onChange={(e) => setCreating({ ...creating, name: e.target.value })}
              />
            </div>
            <div>
              <button type="submit" className="primary">Create</button>{' '}
              <button type="button" onClick={() => setCreating(null)}>Cancel</button>
            </div>
          </form>
        )}
      </Card>

      <Card title="Files">
        {sorted.length === 0 ? (
          <Empty>This folder is empty.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>Name</th><th>Size</th><th>Mode</th><th>Owner</th><th>Modified</th><th />
                </tr>
              </thead>
              <tbody>
                {sorted.map((e) => {
                  const full = path ? `${path}/${e.name}` : e.name;
                  const isRenaming = renaming === full;
                  return (
                    <tr key={e.name}>
                      <td>
                        {isRenaming ? (
                          <RenameInput
                            name={e.name}
                            onCancel={() => setRenaming(null)}
                            onCommit={(to) => {
                              setRenaming(null);
                              if (!to || to === e.name) return;
                              const dir = full.includes('/') ? full.slice(0, full.lastIndexOf('/') + 1) : '';
                              if (editing && editing.path === full) setEditing(null);
                              guard(
                                () => api.post(`/files/rename?${ownerQ}`, { from: full, to: dir + to }),
                                `Renamed to ${to}`,
                              );
                            }}
                          />
                        ) : (
                          <>
                            <span className="muted" style={{ display: 'inline-block', width: '1.3rem' }}>
                              {e.is_link ? '→' : e.is_dir ? '📁' : '📄'}
                            </span>
                            <button
                              type="button"
                              className="link"
                              style={{ fontWeight: e.is_dir ? 500 : 400 }}
                              onClick={() => (e.is_dir ? load(full) : openEditor(full))}
                            >
                              {e.name}
                            </button>
                          </>
                        )}
                      </td>
                      <td className="muted">{e.is_dir ? '—' : fmtBytes(e.size)}</td>
                      <td>
                        <input
                          defaultValue={e.mode}
                          style={{ width: '4.5rem' }}
                          onBlur={(ev) => {
                            if (ev.target.value.trim() === e.mode) return;
                            guard(
                              () => api.post(`/files/chmod?${ownerQ}`, {
                                path: full, mode: ev.target.value.trim(),
                              }),
                              `Permissions set to ${ev.target.value.trim()}`,
                            );
                          }}
                        />
                      </td>
                      <td className="muted">{e.owner}</td>
                      <td className="muted">{fmtDate(e.mod_time)}</td>
                      <td className="right nowrap">
                        {!e.is_dir && (
                          <>
                            <button
                              type="button"
                              className="link"
                              onClick={() => download(`/files/download?${q(`path=${encodeURIComponent(full)}`)}`)}
                            >
                              Download
                            </button>{' '}
                          </>
                        )}
                        <button type="button" className="link" onClick={() => setRenaming(full)}>Rename</button>{' '}
                        <button
                          type="button"
                          className="link"
                          onClick={async () => {
                            const ok = await ask({
                              title: e.is_dir ? `Delete the folder ${e.name}?` : `Delete ${e.name}?`,
                              body: e.is_dir
                                ? 'Everything inside it goes too. This cannot be undone from the panel.'
                                : 'This cannot be undone from the panel.',
                              confirmLabel: 'Delete',
                              danger: true,
                            });
                            if (!ok) return;
                            if (editing && editing.path.startsWith(full)) setEditing(null);
                            guard(
                              () => api.del(`/files?${q(`path=${encodeURIComponent(full)}`)}`),
                              'Deleted',
                            );
                          }}
                        >
                          Delete
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}

        <form
          style={{ marginTop: '.9rem' }}
          onSubmit={(e) => { e.preventDefault(); upload(fileInput.current && fileInput.current.files); }}
          onDragOver={(e) => e.preventDefault()}
          onDrop={(e) => { e.preventDefault(); upload(e.dataTransfer.files); }}
        >
          <div style={{
            border: '1px dashed var(--line)', borderRadius: 'var(--radius)',
            padding: '.9rem', textAlign: 'center',
          }}
          >
            <input type="file" multiple ref={fileInput} style={{ width: 'auto' }} />{' '}
            <button type="submit">Upload here</button>
            <div className="muted" style={{ marginTop: '.4rem', fontSize: '.85rem' }}>
              Up to {fmtBytes(limits.max_upload_bytes)} per file. Larger files belong on SFTP.
            </div>
          </div>
        </form>
      </Card>

      {editing && (
        <Card title={<>Editing <code>{editing.path}</code></>}>
          <textarea
            style={{ minHeight: '24rem', fontFamily: 'ui-monospace, monospace', fontSize: '.85rem' }}
            value={editing.content}
            spellCheck={false}
            onChange={(e) => setEditing({ ...editing, content: e.target.value })}
          />
          <p style={{ marginBottom: 0 }}>
            <button
              type="button"
              className="primary"
              onClick={async () => {
                try {
                  await api.put(`/files/content?${ownerQ}`, {
                    path: editing.path, content: editing.content,
                  });
                  msg.ok(`Saved ${editing.path}`);
                  await load();
                } catch (err) {
                  msg.fail(err);
                }
              }}
            >
              Save
            </button>{' '}
            <button type="button" onClick={() => setEditing(null)}>Close</button>{' '}
            <span className="muted">{fmtBytes(editing.size)} on disk</span>
          </p>
        </Card>
      )}
    </>
  );
}

function RenameInput({ name, onCommit, onCancel }) {
  const [value, setValue] = useState(name);
  return (
    <span style={{ display: 'inline-flex', gap: '.4rem' }}>
      <input
        autoFocus
        style={{ width: '14rem' }}
        value={value}
        onChange={(e) => setValue(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') { e.preventDefault(); onCommit(value.trim()); }
          if (e.key === 'Escape') onCancel();
        }}
      />
      <button type="button" onClick={() => onCommit(value.trim())}>Rename</button>
      <button type="button" onClick={onCancel}>Cancel</button>
    </span>
  );
}
