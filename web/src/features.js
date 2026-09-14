// Every feature, in three groups. `roles` lists who sees an entry; the API
// enforces the same rule, so hiding one is a convenience and never the
// protection.
//
// This drives both the dashboard and the sidebar, which is why it carries a
// line of description for each: on the dashboard that line is the difference
// between a grid of names and a grid somebody can choose from.
export const FEATURES = [
  {
    group: 'Main',
    items: [
      { to: '/sites', label: 'Websites', desc: 'Domains, PHP version and document roots' },
      { to: '/wordpress', label: 'WordPress', desc: 'Core, plugins, themes and one-click sign-in' },
      { to: '/databases', label: 'Databases', desc: 'Databases, accounts and phpMyAdmin' },
      { to: '/files', label: 'Files', desc: 'Browse, edit, upload and unpack' },
      { to: '/backups', label: 'Backups', desc: 'Take, restore and copy off the server' },
      { to: '/ssl', label: 'SSL', desc: 'Certificates for every website' },
      { to: '/security/malware', label: 'Malware', desc: 'Scan files and quarantine what is found' },
      { to: '/cron', label: 'Cron', desc: 'Scheduled commands' },
      { to: '/logs', label: 'Logs', desc: 'Access and error logs per website' },
    ],
  },
  {
    group: 'Server',
    roles: ['admin', 'reseller'],
    items: [
      { to: '/users', label: 'Users', roles: ['admin', 'reseller'], desc: 'Hosting accounts and packages they hold' },
      { to: '/packages', label: 'Packages', roles: ['admin', 'reseller'], desc: 'What an account is allowed' },
      { to: '/import', label: 'Import', roles: ['admin', 'reseller'], desc: 'Bring in a cPanel or DirectAdmin account' },
      { to: '/security/firewall', label: 'Firewall', roles: ['admin'], desc: 'Open ports and blocked addresses' },
      { to: '/security/waf', label: 'WAF', roles: ['admin'], desc: 'The OWASP rule set, per category' },
      { to: '/php', label: 'PHP', roles: ['admin'], desc: 'Versions installed and their settings' },
      { to: '/system', label: 'System', roles: ['admin'], desc: 'Services, updates and the audit trail' },
      { to: '/settings', label: 'Settings', roles: ['admin'], desc: 'Panel address, branding and passkeys' },
    ],
  },
  {
    group: 'Account',
    items: [
      { to: '/account', label: 'Account', desc: 'Password, email, two-factor and passkeys' },
    ],
  },
];
