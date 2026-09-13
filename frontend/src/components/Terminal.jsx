import { useCallback, useEffect, useRef, useState } from 'react';
import { Terminal as XTerminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';

function normalizeOutput(value) {
  return String(value || '').replace(/\r?\n/g, '\r\n');
}

function shortPath(path) {
  const parts = String(path || '').split('/').filter(Boolean);
  if (parts.length >= 2) return `${parts.at(-2)}/${parts.at(-1)}`;
  return parts.at(-1) || '~';
}

function terminalWebSocketUrl(apiBase, websiteId) {
  const base = String(apiBase || '/api');
  const path = `/terminal/ws/${websiteId}`;
  if (/^https?:\/\//i.test(base)) {
    const url = new URL(base);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    url.pathname = `${url.pathname.replace(/\/$/, '')}${path}`;
    url.search = '';
    url.hash = '';
    return url.toString();
  }
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${window.location.host}${base.replace(/\/$/, '')}${path}`;
}

export function Terminal({ websiteId, apiBase = '/api' }) {
  const containerRef = useRef(null);
  const termRef = useRef(null);
  const fitRef = useRef(null);
  const wsRef = useRef(null);
  const lineRef = useRef('');
  const cursorRef = useRef(0);
  const promptRef = useRef('$ ');
  const promptVisibleRef = useRef(false);
  const runningRef = useRef(false);
  const historyRef = useRef([]);
  const historyIndexRef = useRef(-1);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState(null);

  const writePrompt = useCallback(() => {
    termRef.current?.write(promptRef.current);
    promptVisibleRef.current = true;
  }, []);

  const redrawLine = useCallback(() => {
    const term = termRef.current;
    if (!term) return;
    term.write('\r\x1b[K');
    term.write(promptRef.current + lineRef.current);
    const back = lineRef.current.length - cursorRef.current;
    if (back > 0) term.write('\b'.repeat(back));
  }, []);

  const disconnect = useCallback(() => {
    wsRef.current?.close(1000);
    wsRef.current = null;
    setConnected(false);
  }, []);

  const connect = useCallback(() => {
    if (wsRef.current?.readyState === WebSocket.OPEN) return;
    const ws = new WebSocket(terminalWebSocketUrl(apiBase, websiteId));
    wsRef.current = ws;

    ws.onopen = () => {
      setConnected(true);
      setError(null);
      lineRef.current = '';
      cursorRef.current = 0;
      runningRef.current = false;
      promptVisibleRef.current = false;
      termRef.current?.write('\x1b[1;32mConnected\x1b[0m\r\n');
    };

    ws.onmessage = (event) => {
      let msg;
      try {
        msg = JSON.parse(event.data);
      } catch {
        termRef.current?.write(normalizeOutput(event.data));
        return;
      }

      if (msg.type === 'output') {
        termRef.current?.write(normalizeOutput(msg.data));
      } else if (msg.type === 'exit') {
        runningRef.current = false;
        if (Number(msg.code) !== 0) {
          termRef.current?.write(`\r\n\x1b[33m[exit code: ${msg.code}]\x1b[0m\r\n`);
        }
        writePrompt();
      } else if (msg.type === 'cwd') {
        promptRef.current = `${shortPath(msg.data)} $ `;
        if (!runningRef.current && !promptVisibleRef.current) writePrompt();
      } else if (msg.type === 'clear') {
        termRef.current?.clear();
      } else if (msg.type === 'error') {
        termRef.current?.write(`\x1b[31m${normalizeOutput(msg.data)}\x1b[0m\r\n`);
        writePrompt();
      }
    };

    ws.onclose = (event) => {
      setConnected(false);
      wsRef.current = null;
      if (event.code !== 1000) {
        const detail = event.reason ? `${event.code}: ${event.reason}` : `code ${event.code}`;
        setError(`Disconnected (${detail})`);
        termRef.current?.write(`\r\n\x1b[31mDisconnected (${detail})\x1b[0m\r\n`);
      }
    };

    ws.onerror = () => {
      setError('Connection failed');
      setConnected(false);
    };
  }, [apiBase, websiteId, writePrompt]);

  useEffect(() => {
    if (!containerRef.current) return undefined;

    const term = new XTerminal({
      cursorBlink: true,
      cursorStyle: 'block',
      fontSize: 14,
      fontFamily: 'Consolas, "Cascadia Code", "Fira Code", monospace',
      scrollback: 2000,
      // Always-dark surface matching the panel's console tokens (--console-* in
      // style.css); xterm needs literal colours, not vars. Pure neutral, with
      // the cursor in brand blue as the only colour that is not grey.
      theme: {
        background: '#111111',
        foreground: '#f0f0f0',
        cursor: '#4d9ee6',
        selectionBackground: '#3a3a3a',
        black: '#111111',
        red: '#ef4444',
        green: '#22c55e',
        yellow: '#eab308',
        blue: '#60a5fa',
        magenta: '#c084fc',
        cyan: '#2dd4bf',
        white: '#e5e7eb',
        brightBlack: '#6b7280',
        brightRed: '#f87171',
        brightGreen: '#4ade80',
        brightYellow: '#facc15',
        brightBlue: '#93c5fd',
        brightMagenta: '#d8b4fe',
        brightCyan: '#5eead4',
        brightWhite: '#ffffff',
      },
    });

    const fit = new FitAddon();
    fitRef.current = fit;
    term.loadAddon(fit);
    term.open(containerRef.current);
    termRef.current = term;

    let resizeFrame = 0;
    const fitTerminal = () => {
      resizeFrame = 0;
      fit.fit();
      if (wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
      }
    };
    const scheduleFit = () => {
      if (resizeFrame) window.cancelAnimationFrame(resizeFrame);
      resizeFrame = window.requestAnimationFrame(fitTerminal);
    };
    const resizeObserver = typeof ResizeObserver !== 'undefined'
      ? new ResizeObserver(scheduleFit)
      : null;

    resizeObserver?.observe(containerRef.current);
    scheduleFit();
    const onResize = scheduleFit;
    window.addEventListener('resize', onResize);

    term.onData((data) => {
      const ws = wsRef.current;
      if (!ws || ws.readyState !== WebSocket.OPEN) {
        if (data === '\r') term.write('\r\n\x1b[31mNot connected.\x1b[0m\r\n');
        return;
      }

      if (data === '\r' || data === '\n') {
        submitLine();
        return;
      }

      // Multi-line paste: split on newlines and submit each line
      if (data.length > 1 && /[\r\n]/.test(data)) {
        const segments = data.replace(/\r\n/g, '\n').replace(/\r/g, '\n').split('\n');
        for (let i = 0; i < segments.length; i++) {
          const segment = segments[i];
          if (segment) {
            lineRef.current = lineRef.current.slice(0, cursorRef.current) + segment + lineRef.current.slice(cursorRef.current);
            cursorRef.current += segment.length;
            redrawLine();
          }
          if (i < segments.length - 1) {
            submitLine();
          }
        }
        return;
      }

      if (data === '\u007f' || data === '\b') {
        if (cursorRef.current > 0) {
          lineRef.current = lineRef.current.slice(0, cursorRef.current - 1) + lineRef.current.slice(cursorRef.current);
          cursorRef.current -= 1;
          if (cursorRef.current === lineRef.current.length) {
            term.write('\b \b');
          } else {
            redrawLine();
          }
        }
        return;
      }

      if (data === '\x1b[A') {
        if (historyIndexRef.current < historyRef.current.length - 1) {
          historyIndexRef.current += 1;
          lineRef.current = historyRef.current[historyIndexRef.current] || '';
          cursorRef.current = lineRef.current.length;
          redrawLine();
        }
        return;
      }

      if (data === '\x1b[B') {
        if (historyIndexRef.current > 0) {
          historyIndexRef.current -= 1;
          lineRef.current = historyRef.current[historyIndexRef.current] || '';
        } else {
          historyIndexRef.current = -1;
          lineRef.current = '';
        }
        cursorRef.current = lineRef.current.length;
        redrawLine();
        return;
      }

      if (data === '\x1b[D') {
        if (cursorRef.current > 0) {
          cursorRef.current -= 1;
          term.write('\b');
        }
        return;
      }

      if (data === '\x1b[C') {
        if (cursorRef.current < lineRef.current.length) {
          term.write(lineRef.current[cursorRef.current]);
          cursorRef.current += 1;
        }
        return;
      }

      if (data === '\x03') {
        term.write('^C\r\n');
        lineRef.current = '';
        cursorRef.current = 0;
        writePrompt();
        return;
      }

      if (data >= ' ') {
        lineRef.current = lineRef.current.slice(0, cursorRef.current) + data + lineRef.current.slice(cursorRef.current);
        cursorRef.current += data.length;
        if (cursorRef.current === lineRef.current.length) {
          term.write(data);
        } else {
          redrawLine();
        }
      }
    });

    function submitLine() {
      const command = lineRef.current;
      term.write('\r\n');
      if (command.trim()) {
        historyRef.current = [command, ...historyRef.current.filter(item => item !== command)].slice(0, 50);
        historyIndexRef.current = -1;
        lineRef.current = '';
        cursorRef.current = 0;
        runningRef.current = true;
        promptVisibleRef.current = false;
        wsRef.current?.send(JSON.stringify({ type: 'input', data: command }));
      } else {
        lineRef.current = '';
        cursorRef.current = 0;
      }
    }

    connect();

    return () => {
      window.removeEventListener('resize', onResize);
      resizeObserver?.disconnect();
      if (resizeFrame) window.cancelAnimationFrame(resizeFrame);
      disconnect();
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
  }, [connect, disconnect, redrawLine, writePrompt]);

  return (
    <div className="terminal-wrapper">
      <div className="terminal-toolbar">
        <span className="terminal-status">
          {connected ? <span className="status-connected">Connected</span> : error ? <span className="status-error">{error}</span> : <span className="status-disconnected">Disconnected</span>}
        </span>
        {connected ? (
          <button onClick={disconnect} className="terminal-btn disconnect">Disconnect</button>
        ) : (
          <button onClick={connect} className="terminal-btn connect">Connect</button>
        )}
      </div>
      <div className="terminal-container">
        <div ref={containerRef} className="terminal-fit" />
      </div>
      <style>{`
        .terminal-wrapper { display:flex; flex-direction:column; height:100%; background:#17120f; border:1px solid #39302b; border-radius:10px; overflow:hidden; }
        .terminal-toolbar { display:flex; justify-content:space-between; align-items:center; padding:8px 12px; background:#221b17; border-bottom:1px solid #39302b; }
        .terminal-status { font-size:13px; font-family:var(--font-mono); }
        .status-connected { color:#4ade80; }
        .status-disconnected { color:#a2938c; }
        .status-error { color:#f87171; }
        .terminal-btn { padding:5px 12px; border:1px solid transparent; border-radius:6px; font-size:12px; font-weight:600; line-height:1.4; cursor:pointer; transition:background-color .15s ease,color .15s ease,border-color .15s ease; }
        .terminal-btn.connect { background:#ee3124; color:#fff; }
        .terminal-btn.connect:hover { background:#ae271e; }
        .terminal-btn.disconnect { background:transparent; color:#ff8a7d; border-color:rgba(255,138,125,.45); }
        .terminal-btn.disconnect:hover { background:#b42318; color:#fff; border-color:#b42318; }
        .terminal-container { flex:1; min-height:0; padding:8px; overflow:hidden; }
        .terminal-fit { width:100%; height:100%; min-height:0; overflow:hidden; }
        .terminal-fit .xterm { width:100%; height:100%; }
        .terminal-fit .xterm-viewport { overflow-y:auto !important; }
      `}</style>
    </div>
  );
}
