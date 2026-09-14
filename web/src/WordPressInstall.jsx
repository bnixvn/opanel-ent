import React, { useState } from 'react';

const LOCALES = [
  { code: 'en_US', label: 'English (US)' },
  { code: 'vi', label: 'Tiếng Việt' },
  { code: 'en_GB', label: 'English (UK)' },
  { code: 'fr_FR', label: 'Français' },
  { code: 'de_DE', label: 'Deutsch' },
  { code: 'es_ES', label: 'Español' },
  { code: 'ja', label: '日本語' },
  { code: 'zh_CN', label: '简体中文' },
];

// WordPressInstall collects what the installer needs.
//
// One component for all three places it is asked for -- creating a website,
// the button on the websites list, and the WordPress manager -- so the fields
// and the defaults cannot drift apart between them.
//
// Everything is optional. Left empty, the panel picks: the domain becomes the
// title, the owner's name becomes the administrator, the account's email
// becomes the contact, and the password is generated and shown once.
export default function WordPressInstall({ domain, owner, value, onChange, embedded }) {
  const v = value || {};
  const set = (k) => (e) => onChange({ ...v, [k]: e.target.value });

  const body = (
    <>
      <div className="row">
        <div className="field">
          <label htmlFor="wpTitle">Site title</label>
          <input
            id="wpTitle"
            placeholder={domain || 'My website'}
            value={v.title || ''}
            onChange={set('title')}
          />
        </div>
        <div className="field" style={{ flex: '0 0 10rem' }}>
          <label htmlFor="wpLocale">Language</label>
          <select id="wpLocale" value={v.locale || 'en_US'} onChange={set('locale')}>
            {LOCALES.map((l) => <option key={l.code} value={l.code}>{l.label}</option>)}
          </select>
        </div>
      </div>
      <div className="row">
        <div className="field">
          <label htmlFor="wpUser">Administrator username</label>
          <input
            id="wpUser"
            placeholder={owner || 'admin'}
            autoComplete="off"
            value={v.admin_user || ''}
            onChange={set('admin_user')}
          />
        </div>
        <div className="field">
          <label htmlFor="wpEmail">Administrator email</label>
          <input
            id="wpEmail"
            type="email"
            placeholder="you@example.com"
            value={v.admin_email || ''}
            onChange={set('admin_email')}
          />
        </div>
      </div>
      <div className="row">
        <div className="field">
          <label htmlFor="wpPass">Administrator password</label>
          <input
            id="wpPass"
            type="text"
            autoComplete="new-password"
            placeholder="leave empty and one will be generated"
            value={v.admin_password || ''}
            onChange={set('admin_password')}
          />
        </div>
      </div>
      <p className="muted" style={{ margin: '.2rem 0 0', fontSize: '.8rem' }}>
        Anything left empty is filled in for you: the domain becomes the title,
        the hosting account&apos;s name and email become the administrator, and
        a password is generated and shown once. The panel keeps no copy it can
        read back.
      </p>
    </>
  );

  if (embedded) {
    return (
      <div style={{ borderTop: '1px solid var(--line)', marginTop: '.6rem', paddingTop: '.6rem' }}>
        <p style={{ margin: '0 0 .5rem', fontWeight: 600, fontSize: '.88rem' }}>
          WordPress
        </p>
        {body}
      </div>
    );
  }
  return body;
}

// useWordPressInstall keeps the form's state and turns it into a request
// body, dropping the fields the person left alone so the server fills them
// in rather than being handed empty strings.
export function useWordPressInstall() {
  const [value, setValue] = useState({ locale: 'en_US' });
  const body = () => {
    const out = {};
    for (const k of ['title', 'admin_user', 'admin_email', 'admin_password', 'locale']) {
      const s = (value[k] || '').trim();
      if (s) out[k] = s;
    }
    return out;
  };
  return { value, setValue, body, reset: () => setValue({ locale: 'en_US' }) };
}
