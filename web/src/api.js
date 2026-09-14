// One place that talks to the panel.
//
// Every call goes through here so that a session that has expired is handled
// once rather than in forty components, and so that an error carries the
// server's own message instead of "Failed to fetch".

let onUnauthorized = () => {};

// setUnauthorizedHandler lets the app decide what an expired session does.
export function setUnauthorizedHandler(fn) {
  onUnauthorized = fn;
}

export class ApiError extends Error {
  constructor(message, { status, code, details } = {}) {
    super(message);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

async function request(path, { method = 'GET', body, raw, signal } = {}) {
  const opts = { method, signal, headers: {} };

  if (body instanceof FormData) {
    // The browser has to set the multipart boundary itself; naming a
    // Content-Type here would produce a body the server cannot parse.
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }

  const res = await fetch('/api' + path, opts);

  // A 401 from signing in is an answer, not an expiry. The sign-in exchange
  // uses them to say "now the second factor" and "that was wrong", and
  // treating those as a dead session logged the person out of a session they
  // did not have yet -- which is what "your session has ended" meant on a
  // login page, and why enabling passkeys appeared to break signing in.
  //
  // Everywhere else a 401 does mean the session is gone, and the message is
  // worth keeping: it is the only one the user sees when a tab has been open
  // overnight.
  const signingIn = path === '/auth/login';

  if (res.status === 401 && !signingIn) {
    onUnauthorized();
    throw new ApiError('Your session has ended. Sign in again.', { status: 401 });
  }
  if (raw) {
    if (!res.ok) throw new ApiError(res.statusText, { status: res.status });
    return res;
  }

  const text = await res.text();
  let parsed = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = { error: text };
    }
  }
  if (!res.ok) {
    throw new ApiError((parsed && parsed.error) || res.statusText, {
      status: res.status,
      code: parsed && parsed.code,
      details: parsed && parsed.details,
    });
  }
  return parsed;
}

export const api = {
  get: (path, opts) => request(path, opts),
  post: (path, body, opts) => request(path, { ...opts, method: 'POST', body }),
  put: (path, body, opts) => request(path, { ...opts, method: 'PUT', body }),
  patch: (path, body, opts) => request(path, { ...opts, method: 'PATCH', body }),
  del: (path, opts) => request(path, { ...opts, method: 'DELETE' }),
  raw: (path, opts) => request(path, { ...opts, raw: true }),
};

// download navigates rather than fetching: the browser's own machinery
// handles the save dialog and a large file never passes through JavaScript
// memory.
export function download(path) {
  window.location.assign('/api' + path);
}

export function fmtBytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

export function fmtDate(value) {
  if (!value) return '—';
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString();
}
