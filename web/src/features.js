// Every feature, in three groups. `roles` lists who sees an entry; the API
// enforces the same rule, so hiding one is a convenience and never the
// protection.
//
// This drives both the dashboard and the sidebar. `icon` names a glyph in
// icons.jsx; it is what makes the dashboard a grid somebody can aim at
// rather than a wall of words.
export const FEATURES = [
  {
    group: 'Main',
    items: [
      { to: '/sites', label: 'Websites', icon: 'websites' },
      { to: '/wordpress', label: 'WordPress', icon: 'wordpress' },
      { to: '/databases', label: 'Databases', icon: 'databases' },
      { to: '/files', label: 'Files', icon: 'files' },
      { to: '/backups', label: 'Backups', icon: 'backups' },
      { to: '/ssl', label: 'SSL', icon: 'ssl' },
      { to: '/security/malware', label: 'Malware', icon: 'malware' },
      { to: '/cron', label: 'Cron', icon: 'cron' },
      { to: '/logs', label: 'Logs', icon: 'logs' },
    ],
  },
  {
    group: 'Server',
    roles: ['admin', 'reseller'],
    items: [
      { to: '/users', label: 'Users', roles: ['admin', 'reseller'], icon: 'users' },
      { to: '/packages', label: 'Packages', roles: ['admin', 'reseller'], icon: 'packages' },
      { to: '/import', label: 'Import', roles: ['admin', 'reseller'], icon: 'import' },
      { to: '/security/firewall', label: 'Firewall', roles: ['admin'], icon: 'firewall' },
      { to: '/security/waf', label: 'WAF', roles: ['admin'], icon: 'waf' },
      { to: '/php', label: 'PHP', roles: ['admin'], icon: 'php' },
      { to: '/webserver', label: 'Web server', roles: ['admin'], icon: 'webserver' },
      { to: '/cloudlinux', label: 'CloudLinux', roles: ['admin'], icon: 'cloudlinux' },
      { to: '/system', label: 'Service monitor', roles: ['admin'], icon: 'system' },
      { to: '/settings', label: 'Settings', roles: ['admin'], icon: 'settings' },
    ],
  },
  {
    group: 'Account',
    items: [
      { to: '/account/settings', label: 'My account', icon: 'account' },
      { to: '/terminal', label: 'Terminal', icon: 'terminal' },
      { to: '/sftp', label: 'SFTP', icon: 'sftp' },
    ],
  },
];
