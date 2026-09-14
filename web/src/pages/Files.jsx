import React, { Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react';
import { api, download, fmtBytes, fmtDate } from '../api.js';
import { Card, Empty, Hint, Message, useConfirm, useMessage } from '../components.jsx';

// The editor is most of the interface's JavaScript, and it is only needed by
// somebody who has opened a file. Loading it separately keeps the login page
// and every other page small.
const CodeEditor = lazy(() => import('../CodeEditor.jsx'));

// Names an archive is likely to hide behind. Used only to decide whether to
// offer "Extract"; the agent checks the format again before opening anything.
const ARCHIVE_RE = /\.(zip|tar|tar\.gz|tgz)$/i;

export default function Files({ me }) {
  const [owner, setOwner] = useState('');
  const [owners, setOwners] = useState([]);
  const [path, setPath] = useState('');
  const [entries, setEntries] = useState([]);
  const [home, setHome] = useState('');
  const [limits, setLimits] = useState({ max_upload_bytes: 0, max_edit_bytes: 0 });
  const [editing, setEditing] = useState(null);
  const [dirty, setDirty] = useState(false);
  const [renaming, setRenaming] = useState(null);
  const [creating, setCreating] = useState(null);
  const [selected, setSelected] = useState([]);
  const [clipboard, setClipboard] = useState(null);
  const [archiving, setArchiving] = useState(null);
  const [busy, setBusy] = useState(false);
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
      setHome(res.home || '');
      // A selection is a set of paths in the folder that was on screen, so
      // it means nothing once the listing changes.
      setSelected([]);
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
    setBusy(true);
    try {
      await fn();
      msg.ok(okText);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    await load();
  }

  // Directories first, then files: the order people expect when clicking
  // their way down a tree.
  const sorted = [...entries].sort(
    (a, b) => (b.is_dir - a.is_dir) || a.name.localeCompare(b.name),
  );
  const crumbs = path ? path.split('/') : [];
  const fullPath = (name) => (path ? `${path}/${name}` : name);
  const allSelected = sorted.length > 0 && selected.length === sorted.length;

  function toggle(name) {
    setSelected((s) => (s.includes(name) ? s.filter((n) => n !== name) : [...s, name]));
  }

  async function openEditor(full) {
    if (dirty) {
      const ok = await ask({
        title: 'Discard unsaved changes?',
        body: `${editing.path} has changes you have not saved.`,
        confirmLabel: 'Discard them',
        danger: true,
      });
      if (!ok) return;
    }
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
      setDirty(false);
    } catch (err) {
      msg.fail(err);
    }
  }

  async function saveEditor() {
    if (!editing) return;
    try {
      await api.put(`/files/content?${ownerQ}`, {
        path: editing.path, content: editing.content,
      });
      msg.ok(`Saved ${editing.path}`);
      setDirty(false);
      await load();
    } catch (err) {
      msg.fail(err);
    }
  }

  // Paste is a loop rather than one call: each item can fail on its own
  // (a name already taken, say) and the rest should still land.
  async function paste() {
    if (!clipboard || !clipboard.items.length) return;
    msg.clear();
    setBusy(true);
    const failed = [];
    for (const item of clipboard.items) {
      const name = item.slice(item.lastIndexOf('/') + 1);
      const to = path ? `${path}/${name}` : name;
      if (to === item) { failed.push(`${name} (already here)`); continue; }
      try {
        if (clipboard.mode === 'copy') {
          await api.post(`/files/copy?${ownerQ}`, { from: item, to });
        } else {
          await api.post(`/files/rename?${ownerQ}`, { from: item, to });
        }
      } catch (err) {
        failed.push(`${name} (${err.message})`);
      }
    }
    setBusy(false);
    if (failed.length) {
      msg.fail(new Error(`Could not ${clipboard.mode}: ${failed.join('; ')}`));
    } else {
      msg.ok(`${clipboard.mode === 'copy' ? 'Copied' : 'Moved'} ${clipboard.items.length} item(s)`);
    }
    if (clipboard.mode === 'move') setClipboard(null);
    await load();
  }

  async function deleteSelected() {
    const ok = await ask({
      title: `Delete ${selected.length} item(s)?`,
      body: 'Any folder in the selection goes with everything inside it. '
        + 'This cannot be undone from the panel.',
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) return;
    msg.clear();
    setBusy(true);
    const failed = [];
    for (const name of selected) {
      try {
        await api.del(`/files?${q(`path=${encodeURIComponent(fullPath(name))}`)}`);
      } catch (err) {
        failed.push(`${name} (${err.message})`);
      }
    }
    setBusy(false);
    if (failed.length) msg.fail(new Error(`Could not delete: ${failed.join('; ')}`));
    else msg.ok(`Deleted ${selected.length} item(s)`);
    if (editing && selected.some((n) => editing.path.startsWith(fullPath(n)))) setEditing(null);
    await load();
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
                onChange={(e) => {
                  setOwner(e.target.value);
                  setEditing(null);
                  setClipboard(null);
                  load('', e.target.value);
                }}
              >
                {owners.length === 0 && <option value="">no hosting accounts yet</option>}
                {owners.map((u) => <option key={u.id}>{u.username}</option>)}
              </select>
            </div>
          )}
          <div className="grow">
            <label>Location</label>
            <div style={{ fontFamily: 'ui-monospace, monospace', fontSize: '.85rem' }}>
              <button type="button" className="link" onClick={() => load('')}>
                {home || '/home'}
              </button>
              {crumbs.map((c, i) => (
                <span key={`${c}-${i}`}>
                  <span className="muted">/</span>
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

        {/* The selection bar. Present only when something is selected, so it
            never competes with the navigation above it. */}
        {(selected.length > 0 || clipboard) && (
          <div className="selbar">
            {selected.length > 0 && (
              <>
                <strong>{selected.length} selected</strong>
                <button type="button" disabled={busy} onClick={() => setClipboard({ mode: 'copy', items: selected.map(fullPath) })}>
                  Copy
                </button>
                <button type="button" disabled={busy} onClick={() => setClipboard({ mode: 'move', items: selected.map(fullPath) })}>
                  Cut
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => setArchiving({
                    name: (selected.length === 1 ? selected[0] : 'archive') + '.zip',
                  })}
                >
                  Compress
                </button>
                <button type="button" disabled={busy} className="danger" onClick={deleteSelected}>
                  Delete
                </button>
                <button type="button" className="link" onClick={() => setSelected([])}>Clear</button>
              </>
            )}
            {clipboard && (
              <span style={{ marginLeft: 'auto' }} className="nowrap">
                <span className="muted">
                  {clipboard.items.length} item(s) ready to {clipboard.mode === 'copy' ? 'copy' : 'move'}
                </span>{' '}
                <button type="button" className="primary" disabled={busy} onClick={paste}>
                  Paste here
                </button>{' '}
                <button type="button" className="link" onClick={() => setClipboard(null)}>Cancel</button>
              </span>
            )}
          </div>
        )}

        {archiving && (
          <form
            className="row"
            onSubmit={(e) => {
              e.preventDefault();
              const dest = fullPath(archiving.name.trim());
              const paths = selected.map(fullPath);
              setArchiving(null);
              guard(
                () => api.post(`/files/archive?${ownerQ}`, { paths, dest }),
                `Created ${archiving.name.trim()}`,
              );
            }}
          >
            <div className="field" style={{ flex: '0 1 20rem' }}>
              <label htmlFor="arcName">
                Archive name
                <Hint>
                  The extension picks the format: .zip, .tar.gz or .tar.
                </Hint>
              </label>
              <input
                id="arcName"
                autoFocus
                required
                value={archiving.name}
                onChange={(e) => setArchiving({ name: e.target.value })}
              />
            </div>
            <div>
              <button type="submit" className="primary">Compress</button>{' '}
              <button type="button" onClick={() => setArchiving(null)}>Cancel</button>
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
                  <th style={{ width: '1.6rem' }}>
                    <input
                      type="checkbox"
                      aria-label="Select everything in this folder"
                      checked={allSelected}
                      onChange={() => setSelected(allSelected ? [] : sorted.map((e) => e.name))}
                    />
                  </th>
                  <th>Name</th><th>Size</th><th>Mode</th><th>Owner</th><th>Modified</th><th />
                </tr>
              </thead>
              <tbody>
                {sorted.map((e) => {
                  const full = fullPath(e.name);
                  const isRenaming = renaming === full;
                  return (
                    <tr key={e.name} className={selected.includes(e.name) ? 'selected' : undefined}>
                      <td>
                        <input
                          type="checkbox"
                          aria-label={`Select ${e.name}`}
                          checked={selected.includes(e.name)}
                          onChange={() => toggle(e.name)}
                        />
                      </td>
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
                      <td className="nowrap">
                        <input
                          defaultValue={e.mode}
                          style={{ width: '4.5rem', display: 'inline-block' }}
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
                        {e.is_dir && (
                          <ChmodRecursive
                            onApply={(mode) => guard(
                              () => api.post(`/files/chmod-recursive?${ownerQ}`, { path: full, mode }),
                              `${e.name} and everything in it set to ${mode}`,
                            )}
                            ask={ask}
                            name={e.name}
                          />
                        )}
                      </td>
                      <td className="muted">{e.owner}</td>
                      <td className="muted">{fmtDate(e.mod_time)}</td>
                      <td className="right nowrap">
                        {!e.is_dir && ARCHIVE_RE.test(e.name) && (
                          <>
                            <button
                              type="button"
                              className="link"
                              disabled={busy}
                              onClick={async () => {
                                const ok = await ask({
                                  title: `Extract ${e.name} here?`,
                                  body: 'Files already in this folder with the same names '
                                    + 'are replaced. Entries that point outside the folder '
                                    + 'are refused rather than written.',
                                  confirmLabel: 'Extract',
                                });
                                if (!ok) return;
                                setBusy(true);
                                try {
                                  const res = await api.post(`/files/extract?${ownerQ}`, {
                                    path: full, dest: path,
                                  });
                                  msg.ok(`Extracted ${res.entries} item(s), ${fmtBytes(res.bytes)}`);
                                } catch (err) {
                                  msg.fail(err);
                                }
                                setBusy(false);
                                await load();
                              }}
                            >
                              Extract
                            </button>{' '}
                          </>
                        )}
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
              Up to {fmtBytes(limits.max_upload_bytes)} per file.
            </div>
          </div>
        </form>
      </Card>

      {editing && (
        <Card
          title={<>Editing <code>{editing.path}</code>{dirty ? ' •' : ''}</>}
          actions={(
            <>
              <button type="button" className="primary" onClick={saveEditor} disabled={!dirty}>
                Save
              </button>{' '}
              <button
                type="button"
                onClick={async () => {
                  if (dirty) {
                    const ok = await ask({
                      title: 'Close without saving?',
                      body: `${editing.path} has changes you have not saved.`,
                      confirmLabel: 'Discard them',
                      danger: true,
                    });
                    if (!ok) return;
                  }
                  setEditing(null);
                  setDirty(false);
                }}
              >
                Close
              </button>
            </>
          )}
        >
          <Suspense fallback={<p className="muted">Loading the editor…</p>}>
            <CodeEditor
              value={editing.content}
              filename={editing.path}
              onChange={(text) => {
                editing.content = text; // eslint-disable-line no-param-reassign
                if (!dirty) setDirty(true);
              }}
              onSave={saveEditor}
            />
          </Suspense>
          <p className="muted" style={{ margin: '.5rem 0 0', fontSize: '.8rem' }}>
            {fmtBytes(editing.size)} on disk.
          </p>
        </Card>
      )}
    </>
  );
}

// ChmodRecursive is deliberately two clicks and a confirmation.
//
// Getting it wrong on a home directory is one of the few things in the file
// manager a customer cannot undo themselves: a tree with no execute bit on
// its directories cannot be listed, so it cannot be repaired through this
// page either.
function ChmodRecursive({ name, onApply, ask }) {
  const [open, setOpen] = useState(false);
  const [mode, setMode] = useState('0755');

  if (!open) {
    return (
      <>
        {' '}
        <button type="button" className="link" title="Apply a mode to everything inside" onClick={() => setOpen(true)}>
          ⇊
        </button>
      </>
    );
  }
  return (
    <span style={{ display: 'inline-flex', gap: '.3rem', marginLeft: '.3rem' }}>
      <input value={mode} style={{ width: '4.5rem' }} onChange={(e) => setMode(e.target.value)} />
      <button
        type="button"
        onClick={async () => {
          const ok = await ask({
            title: `Apply ${mode} to everything in ${name}?`,
            body: 'Every file inside gets this mode, and every folder gets it '
              + 'plus the execute bit it needs to be opened at all. There is no '
              + 'undo, and the panel does not record what the modes were.',
            confirmLabel: 'Apply',
            danger: true,
          });
          setOpen(false);
          if (ok) onApply(mode.trim());
        }}
      >
        Apply to all
      </button>
      <button type="button" className="link" onClick={() => setOpen(false)}>×</button>
    </span>
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
