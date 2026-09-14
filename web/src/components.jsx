import React, { useEffect, useState } from 'react';

// Small pieces every page uses. Kept together because each is a few lines
// and a file per component would be more navigation than code.

export function Card({ title, children, actions }) {
  return (
    <section className="card">
      {(title || actions) && (
        <div className="cardhead">
          {title && <h2>{title}</h2>}
          {actions && <div className="cardactions">{actions}</div>}
        </div>
      )}
      {children}
    </section>
  );
}

export function Tag({ kind = 'mute', children }) {
  return <span className={`tag ${kind}`}>{children}</span>;
}

// Message shows the outcome of the last action. Success fades, failure does
// not: a customer who looked away must still find out the delete failed.
export function Message({ value, onClear }) {
  useEffect(() => {
    if (!value || value.kind !== 'ok') return undefined;
    const t = setTimeout(() => onClear && onClear(), 5000);
    return () => clearTimeout(t);
  }, [value, onClear]);

  if (!value || !value.text) return null;
  return <div className={`msg ${value.kind || 'err'}`}>{value.text}</div>;
}

export function useMessage() {
  const [message, setMessage] = useState(null);
  return {
    message,
    clear: () => setMessage(null),
    ok: (text) => setMessage({ kind: 'ok', text }),
    warn: (text) => setMessage({ kind: 'warn', text }),
    fail: (err) => setMessage({ kind: 'err', text: err && err.message ? err.message : String(err) }),
  };
}

export function Empty({ children }) {
  return <div className="empty">{children}</div>;
}

// Search filters a list in the browser. The endpoints also accept ?q= for
// servers with more rows than are worth sending, and the page switches to
// that when the list gets long.
export function Search({ value, onChange, placeholder }) {
  return (
    <input
      className="search"
      type="search"
      value={value}
      placeholder={placeholder || 'Search…'}
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

export function matches(haystack, needle) {
  if (!needle) return true;
  return String(haystack || '').toLowerCase().includes(needle.toLowerCase());
}

// Confirm is a deliberate replacement for window.confirm: the browser dialog
// blocks the whole page, cannot be styled, and cannot explain what is about
// to happen in more than one paragraph.
export function Confirm({ open, title, body, confirmLabel, danger, onConfirm, onCancel }) {
  if (!open) return null;
  return (
    <div
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,.45)',
        display: 'grid', placeItems: 'center', zIndex: 50, padding: '1rem',
      }}
      onClick={onCancel}
    >
      <div
        className="card"
        style={{ maxWidth: '32rem', margin: 0 }}
        onClick={(e) => e.stopPropagation()}
      >
        <h2>{title}</h2>
        <div style={{ fontSize: '.9rem', whiteSpace: 'pre-line' }}>{body}</div>
        <div style={{ marginTop: '1rem', display: 'flex', gap: '.6rem', justifyContent: 'flex-end' }}>
          <button type="button" onClick={onCancel}>Cancel</button>
          <button type="button" className={danger ? 'danger' : 'primary'} onClick={onConfirm}>
            {confirmLabel || 'Confirm'}
          </button>
        </div>
      </div>
    </div>
  );
}

// useConfirm gives a page one dialog it can raise from anywhere.
export function useConfirm() {
  const [state, setState] = useState(null);
  const ask = (opts) =>
    new Promise((resolve) => {
      setState({
        ...opts,
        onConfirm: () => { setState(null); resolve(true); },
        onCancel: () => { setState(null); resolve(false); },
      });
    });
  const dialog = <Confirm open={!!state} {...(state || {})} />;
  return { ask, dialog };
}

// Secret shows a password once, with a copy button, and makes clear it will
// not be shown again.
export function Secret({ label, value }) {
  const [copied, setCopied] = useState(false);
  if (!value) return null;
  return (
    <div style={{ margin: '.4rem 0' }}>
      <label>{label}</label>
      <div style={{ display: 'flex', gap: '.4rem', alignItems: 'center' }}>
        <code style={{ flex: 1, wordBreak: 'break-all' }}>{value}</code>
        <button
          type="button"
          onClick={() => {
            navigator.clipboard.writeText(value).then(
              () => { setCopied(true); setTimeout(() => setCopied(false), 2000); },
              () => {},
            );
          }}
        >
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
    </div>
  );
}

// Hint is an explanation somebody can ask for.
//
// Printed under every field, the same words become noise that hides the ones
// that matter. As a tooltip they stay one keystroke away, and because the
// text is in the markup rather than a title attribute it is still read out
// by a screen reader and still found by a page search.
export function Hint({ children }) {
  if (!children) return null;
  return (
    <i className="hint" tabIndex={0} role="note">
      ?
      <span>{children}</span>
    </i>
  );
}
