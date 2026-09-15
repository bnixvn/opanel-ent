import React from 'react';

// Line icons, drawn here rather than pulled from a package.
//
// An icon set is a few hundred kilobytes to get the eighteen glyphs this
// panel uses, and the panel is served from the server it administers -- on a
// box somebody is trying to rescue, a login page that waits on a font or a
// CDN is a login page that does not load. These are a couple of kilobytes in
// the bundle that is already being fetched.
const PATHS = {
  websites: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M3 12h18" />
      <path d="M12 3a15 15 0 0 1 0 18a15 15 0 0 1 0-18" />
    </>
  ),
  wordpress: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M6.6 8.2l2.6 8 2.3-6.4" />
      <path d="M12 12.4l1.9 3.8 2.9-8" />
    </>
  ),
  databases: (
    <>
      <ellipse cx="12" cy="6" rx="7" ry="3" />
      <path d="M5 6v12c0 1.7 3.1 3 7 3s7-1.3 7-3V6" />
      <path d="M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3" />
    </>
  ),
  files: <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />,
  backups: (
    <>
      <path d="M3 6h18v4H3z" />
      <path d="M5 10v9h14v-9" />
      <path d="M10 14h4" />
    </>
  ),
  ssl: (
    <>
      <rect x="5" y="11" width="14" height="9" rx="2" />
      <path d="M8 11V8a4 4 0 0 1 8 0v3" />
    </>
  ),
  malware: (
    <>
      <path d="M12 3l7 3v6c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" />
      <path d="M12 9v4" />
      <path d="M12 16h.01" />
    </>
  ),
  cron: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3.5 2" />
    </>
  ),
  logs: (
    <>
      <path d="M4 6h16" />
      <path d="M4 12h16" />
      <path d="M4 18h10" />
    </>
  ),
  users: (
    <>
      <circle cx="9" cy="8" r="3.5" />
      <path d="M2.5 20c0-3.3 2.9-5.5 6.5-5.5s6.5 2.2 6.5 5.5" />
      <path d="M17 5.6a3.4 3.4 0 0 1 0 6.4" />
      <path d="M18.5 14.8c1.9.7 3 2.2 3 4.2" />
    </>
  ),
  packages: (
    <>
      <path d="M12 3l8 4.5v9L12 21l-8-4.5v-9z" />
      <path d="M4 7.5l8 4.5 8-4.5" />
      <path d="M12 12v9" />
    </>
  ),
  import: (
    <>
      <path d="M12 3v10" />
      <path d="M8 9l4 4 4-4" />
      <path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2" />
    </>
  ),
  firewall: (
    <>
      <rect x="3" y="5" width="18" height="14" rx="1.5" />
      <path d="M3 10h18" />
      <path d="M3 15h18" />
      <path d="M9 5v5" />
      <path d="M15 10v5" />
      <path d="M9 15v4" />
    </>
  ),
  waf: (
    <>
      <path d="M12 3l7 3v6c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" />
      <path d="M9 12l2 2 4-4" />
    </>
  ),
  php: (
    <>
      <path d="M9 7l-5 5 5 5" />
      <path d="M15 7l5 5-5 5" />
    </>
  ),
  cloudlinux: (
    <>
      <path d="M7 18h9.5a3.5 3.5 0 0 0 .5-6.96A5 5 0 0 0 7.5 9.5 3.5 3.5 0 0 0 7 18z" />
      <path d="M12 12.5v4" />
      <path d="M10 14.5l2-2 2 2" />
    </>
  ),
  webserver: (
    <>
      <rect x="3" y="4" width="18" height="6" rx="1.5" />
      <rect x="3" y="14" width="18" height="6" rx="1.5" />
      <path d="M6.5 7h.01" />
      <path d="M6.5 17h.01" />
      <path d="M10 7h7" />
      <path d="M10 17h7" />
    </>
  ),
  system: (
    <>
      <rect x="4.5" y="4.5" width="15" height="15" rx="2" />
      <rect x="9.5" y="9.5" width="5" height="5" rx="1" />
      <path d="M9 2v2.5" />
      <path d="M15 2v2.5" />
      <path d="M9 19.5V22" />
      <path d="M15 19.5V22" />
      <path d="M2 9h2.5" />
      <path d="M2 15h2.5" />
      <path d="M19.5 9H22" />
      <path d="M19.5 15H22" />
    </>
  ),
  settings: (
    <>
      <path d="M4 8h9" />
      <path d="M19 8h1" />
      <path d="M4 16h4" />
      <path d="M14 16h6" />
      <circle cx="16" cy="8" r="2.4" />
      <circle cx="11" cy="16" r="2.4" />
    </>
  ),
  terminal: (
    <>
      <rect x="3" y="4.5" width="18" height="15" rx="2" />
      <path d="M7 9.5l3 2.5-3 2.5" />
      <path d="M12.5 15h4" />
    </>
  ),
  sftp: (
    <>
      <path d="M12 3v9" />
      <path d="M8.5 8.5L12 12l3.5-3.5" />
      <path d="M4 14v4a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-4" />
      <path d="M7 17h.01" />
    </>
  ),
  account: (
    <>
      <circle cx="12" cy="12" r="9" />
      <circle cx="12" cy="10" r="3" />
      <path d="M6.3 18.7a7 7 0 0 1 11.4 0" />
    </>
  ),
};

export default function Icon({ name, size = 20 }) {
  const glyph = PATHS[name];
  if (!glyph) return null;
  return (
    <svg
      className="icon"
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {glyph}
    </svg>
  );
}
