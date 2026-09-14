import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Terminal as Xterm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';

// Terminal is a shell as the account's own Linux user.
//
// Loaded on its own chunk. xterm is a quarter of a megabyte, and a login
// page that waits for a terminal emulator nobody has asked for is a login
// page that is slow on the one day the server is in trouble.

export default function Terminal({ username, onClose }) {
  const host = useRef(null);
  const [state, setState] = useState('connecting');
  const [error, setError] = useState('');

  // Kept out of state: changing either of these must not re-render, and
  // re-rendering must not throw either of them away.
  const term = useRef(null);
  const socket = useRef(null);

  const send = useCallback((data) => {
    const ws = socket.current;
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(data);
  }, []);

  useEffect(() => {
    const xterm = new Xterm({
      convertEol: false,
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      fontSize: 13,
      scrollback: 5000,
      theme: terminalTheme(),
    });
    const fit = new FitAddon();
    xterm.loadAddon(fit);
    xterm.open(host.current);
    fit.fit();
    term.current = xterm;

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const query = new URLSearchParams({
      cols: String(xterm.cols),
      rows: String(xterm.rows),
    });
    if (username) query.set('user', username);
    const ws = new WebSocket(`${proto}//${window.location.host}/api/terminal?${query}`);
    ws.binaryType = 'arraybuffer';
    socket.current = ws;

    const decoder = new TextDecoder();
    const encoder = new TextEncoder();

    ws.onopen = () => setState('open');
    ws.onmessage = (ev) => {
      xterm.write(typeof ev.data === 'string' ? ev.data : decoder.decode(ev.data));
    };
    ws.onerror = () => {
      setError('The connection failed. The agent may not be running.');
      setState('closed');
    };
    ws.onclose = (ev) => {
      setState('closed');
      if (ev.reason) xterm.write(`\r\n\x1b[2m— ${ev.reason} —\x1b[0m\r\n`);
    };

    const typed = xterm.onData((data) => send(encoder.encode(data)));

    // The shell has to be told the window changed, or a full-screen program
    // draws for the size it was given at startup and nothing else.
    const resize = () => {
      try { fit.fit(); } catch { /* the element is not laid out yet */ }
      const ws2 = socket.current;
      if (ws2 && ws2.readyState === WebSocket.OPEN) {
        ws2.send(JSON.stringify({ resize: { cols: xterm.cols, rows: xterm.rows } }));
      }
    };
    const observer = new ResizeObserver(resize);
    observer.observe(host.current);
    window.addEventListener('resize', resize);

    return () => {
      observer.disconnect();
      window.removeEventListener('resize', resize);
      typed.dispose();
      try { ws.close(); } catch { /* already gone */ }
      xterm.dispose();
      socket.current = null;
      term.current = null;
    };
  }, [username, send]);

  return (
    <div className="terminal">
      <div className="terminalbar">
        <span className={`dot ${state}`} />
        <span className="muted">
          {state === 'open' ? `Connected as ${username}`
            : state === 'connecting' ? 'Connecting…' : 'Disconnected'}
        </span>
        <span className="spacer" />
        <button type="button" onClick={onClose}>Close</button>
      </div>
      {error && <div className="msg err">{error}</div>}
      <div className="terminalbody" ref={host} />
    </div>
  );
}

// terminalTheme follows the panel's own light or dark setting, so opening a
// shell is not a white rectangle in the middle of a dark page.
function terminalTheme() {
  const dark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  return dark
    ? { background: '#0f1115', foreground: '#e6e8eb', cursor: '#4b83f0' }
    : { background: '#16191d', foreground: '#e6e8eb', cursor: '#8ab4ff' };
}
