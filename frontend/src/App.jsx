import React, { useEffect, useState, useCallback, useRef } from 'react';
import { createRoot } from 'react-dom/client';
import ace from 'ace-builds/src-noconflict/ace';
import 'ace-builds/src-noconflict/ext-language_tools';
import 'ace-builds/src-noconflict/ext-searchbox';
import 'ace-builds/src-noconflict/mode-css';
import 'ace-builds/src-noconflict/mode-html';
import 'ace-builds/src-noconflict/mode-ini';
import 'ace-builds/src-noconflict/mode-javascript';
import 'ace-builds/src-noconflict/mode-json';
import 'ace-builds/src-noconflict/mode-php';
import 'ace-builds/src-noconflict/mode-text';
import 'ace-builds/src-noconflict/mode-yaml';
import 'ace-builds/src-noconflict/theme-textmate';
import 'ace-builds/src-noconflict/theme-tomorrow_night';
import { Archive, ArrowLeft, Check, CheckCircle, ChevronDown, Clock, Code2, Copy, Cpu, Database, Dices, FileText, FolderOpen, Globe, HardDrive, Home, Image, KeyRound, Lock, LogIn, LogOut, MemoryStick, Menu, Moon, MoveRight, Network, PackageOpen, Pencil, Save, Search, Server, Settings as SettingsIcon, Shield, Sun, Trash2, TerminalIcon, Users, X, RefreshCw, Plus, Download, Upload, Play, Square, RotateCcw, AlertCircle, Zap, ExternalLink, Ban } from 'lucide-react';
import { Terminal } from './components/Terminal';
import './style.css';
import './brand.css';
import './file-manager.css';

const API = import.meta.env.VITE_API_URL || '/api';
const DEFAULT_SERVICE_NAMES = ['opanel-ent-api', 'nginx', 'php8.3-fpm', 'php8.4-fpm', 'mariadb', 'redis-server'];
const PHP_VERSION_ORDER = ['7.4', '8.1', '8.2', '8.3', '8.4', '8.5'];
const NGINX_REWRITE_MODES = [
  { value: 'none', label: 'None / static PHP' },
  { value: 'front_controller', label: 'PHP front controller' },
  { value: 'laravel', label: 'Laravel' },
  { value: 'codeigniter', label: 'CodeIgniter' },
  { value: 'seohburl', label: 'SEO HB URL' },
];
const SETTINGS_PAGE_KEYS = ['settings', 'security', 'malware', 'php', 'firewall', 'waf', 'wafLogs', 'updates', 'services'];
const WEEKDAY_LABELS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const PAGE_ROUTES = {
  dashboard: '/',
  websites: '/website',
  ssl: '/ssl',
  databases: '/database',
  cron: '/cron',
  files: '/filemanager',
  backups: '/backups',
  users: '/users',
  settings: '/settings',
  security: '/security',
  malware: '/malware',
  php: '/php',
  firewall: '/firewall',
  waf: '/waf',
  wafLogs: '/waf-logs',
  updates: '/updates',
  services: '/services',
};
const ROUTE_PAGES = new Map([
  ...Object.entries(PAGE_ROUTES).map(([pageName, path]) => [path, pageName]),
  ['/dashboard', 'dashboard'],
  ['/websites', 'websites'],
  ['/databases', 'databases'],
  ['/files', 'files'],
  ['/file-manager', 'files'],
  ['/website', 'websites'],
]);

function pageFromPathname(pathname) {
  const normalized = `/${String(pathname || '').replace(/^\/+|\/+$/g, '')}`.toLowerCase();
  return ROUTE_PAGES.get(normalized) || 'dashboard';
}

function routeForPage(pageName) {
  return PAGE_ROUTES[pageName] || PAGE_ROUTES.dashboard;
}

function phpVersionOptions(installed = [], current = '') {
  const list = sortPhpVersions(installed);
  if (current && !list.includes(current)) return sortPhpVersions([...list, current]);
  return list;
}

function sortPhpVersions(versions = []) {
  return [...versions].sort((a, b) => {
    const ai = PHP_VERSION_ORDER.indexOf(a);
    const bi = PHP_VERSION_ORDER.indexOf(b);
    if (ai !== -1 || bi !== -1) return (ai === -1 ? 999 : ai) - (bi === -1 ? 999 : bi);
    return String(a).localeCompare(String(b), undefined, { numeric: true });
  });
}

function websiteConfigForm(site = {}) {
  const appType = site.app_type || 'wordpress';
  return {
    app_type: appType,
    php_version: site.php_version || '8.4',
    nginx_rewrite_mode: appType === 'wordpress'
      ? 'front_controller'
      : appType === 'static'
        ? 'none'
        : site.nginx_rewrite_mode || 'none',
  };
}

function editorParamsFromLocation() {
  const params = new URLSearchParams(window.location.search);
  if (params.get('view') !== 'editor') return null;
  const websiteId = params.get('website_id');
  const path = params.get('path') || 'public_html/index.html';
  if (!websiteId) return null;
  return { websiteId: String(websiteId), path };
}

function aceModeName(mode) {
  if (mode === 'PHP') return 'php';
  if (mode === 'JavaScript') return 'javascript';
  if (mode === 'CSS') return 'css';
  if (mode === 'HTML') return 'html';
  if (mode === 'JSON') return 'json';
  if (mode === 'YAML') return 'yaml';
  if (mode === 'Config') return 'ini'; // .env, .htaccess, .ini, .conf -> Ace's ini mode
  return 'text';
}

function formatLogCount(value = 0) {
  const count = Number(value) || 0;
  if (count >= 1000) return `${(count / 1000).toFixed(count >= 10000 ? 0 : 1)}k`;
  return String(count);
}

function formatLogDuration(value = 0) {
  const duration = Number(value);
  return `${Number.isFinite(duration) ? duration : 0} ms`;
}

function csvEscape(value) {
  const text = String(value ?? '');
  return /[",\n\r]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
}

function formatApiError(detail, fallback = 'Request failed.') {
  if (detail === null || detail === undefined || detail === '') return fallback;
  if (typeof detail === 'string') return detail.replace(/^Value error,\s*/i, '') || fallback;
  if (typeof detail === 'number' || typeof detail === 'boolean') return String(detail);

  if (Array.isArray(detail)) {
    const messages = detail.map(item => formatApiErrorItem(item)).filter(Boolean);
    return messages.length ? messages.join('\n') : fallback;
  }

  if (typeof detail === 'object') {
    if (detail.detail !== undefined) return formatApiError(detail.detail, fallback);
    if (detail.message !== undefined) return formatApiError(detail.message, fallback);
    if (detail.msg !== undefined) return formatApiError(detail.msg, fallback);
    try { return JSON.stringify(detail); } catch { return fallback; }
  }

  return fallback;
}

function formatApiErrorItem(item) {
  if (!item || typeof item !== 'object') return formatApiError(item, '');
  const message = formatApiError(item.msg ?? item.message ?? item.detail, 'Invalid value');
  const loc = Array.isArray(item.loc)
    ? item.loc.filter(part => part !== 'body' && part !== 'query' && part !== 'path').join('.')
    : '';
  return loc ? `${loc}: ${message}` : message;
}

function NotificationToast({ type, message, onClose }) {
  if (!message) return null;
  const isError = type === 'error';
  const Icon = isError ? AlertCircle : Check;
  return <div className={`app-toast ${isError ? 'app-toast-error' : 'app-toast-success'}`} role={isError ? 'alert' : 'status'} aria-live={isError ? 'assertive' : 'polite'}>
    <Icon className="app-toast-icon" size={18}/>
    <div className="app-toast-content">
      <strong>{isError ? 'Action failed' : 'Completed'}</strong>
      <span>{message}</span>
    </div>
    <button className="app-toast-close" onClick={onClose} aria-label="Dismiss notification" title="Dismiss notification"><X size={16}/></button>
  </div>;
}

function isHostnameDomain(value = '') {
  return /^(?!-)([a-z0-9-]{1,63}\.)+[a-z]{2,}$/i.test(String(value).trim());
}

function defaultPanelSslEmail(hostname = '') {
  const host = String(hostname || '').trim().toLowerCase();
  return isHostnameDomain(host) ? `admin@${host}` : '';
}

function copyToClipboard(text) {
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(() => {}, () => fallbackCopy(text));
  } else {
    fallbackCopy(text);
  }
}
function fallbackCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.cssText = 'position:fixed;left:-9999px;top:-9999px';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); } catch {}
  document.body.removeChild(ta);
}

function aceThemePath(theme) {
  return theme === 'dark' ? 'ace/theme/tomorrow_night' : 'ace/theme/textmate';
}

function CodeEditor({ value, mode, disabled, theme, onChange, onCursorChange }) {
  const hostRef = useRef(null);
  const editorRef = useRef(null);
  const suppressChangeRef = useRef(false);
  const onChangeRef = useRef(onChange);
  const onCursorChangeRef = useRef(onCursorChange);

  useEffect(() => { onChangeRef.current = onChange; }, [onChange]);
  useEffect(() => { onCursorChangeRef.current = onCursorChange; }, [onCursorChange]);

  // Mount once. Sizing, font metrics and scrolling stay Ace's job: it renders
  // only the visible rows and drives its own scrollbars, so re-implementing any
  // of that desyncs the renderer from the document (last lines unreachable,
  // duplicate scrollbars). Height and colours come from CSS instead.
  useEffect(() => {
    if (!hostRef.current) return undefined;
    const editor = ace.edit(hostRef.current, {
      mode: `ace/mode/${aceModeName(mode)}`,
      theme: aceThemePath(theme),
      value: value || '',
      readOnly: !!disabled,
      showPrintMargin: false,
      highlightActiveLine: true,
      // Ace stamps font-size on the host inline (it defaults the option), so
      // CSS cannot set it — both font properties live here and only the line
      // height stays in CSS, which Ace measures off the DOM. var() keeps the
      // token the single source of truth for the stack.
      fontSize: 13,
      fontFamily: 'var(--font-mono)',
      tabSize: 2,
      useSoftTabs: true,
      wrap: false,
      selectionStyle: 'text',
      enableBasicAutocompletion: true,
      enableLiveAutocompletion: true,
      enableSnippets: false,
    });
    editor.session.setUseWorker(false);
    editor.session.setNewLineMode('unix');

    const reportCursor = () => {
      const pos = editor.getCursorPosition();
      onCursorChangeRef.current?.({ line: pos.row + 1, column: pos.column + 1 });
    };
    const handleChange = () => {
      if (suppressChangeRef.current) return;
      onChangeRef.current?.(editor.getValue());
    };

    editor.session.on('change', handleChange);
    editor.selection.on('changeCursor', reportCursor);
    editorRef.current = editor;
    reportCursor();

    return () => {
      editorRef.current = null;
      editor.destroy();
    };
  }, []);

  useEffect(() => {
    const editor = editorRef.current;
    if (!editor) return;
    const nextValue = value || '';
    if (nextValue === editor.getValue()) return;
    const cursor = editor.getCursorPosition();
    suppressChangeRef.current = true;
    editor.setValue(nextValue, -1);
    const newRow = Math.max(0, Math.min(cursor.row, editor.session.getLength() - 1));
    editor.moveCursorTo(newRow, cursor.column);
    suppressChangeRef.current = false;
  }, [value]);

  useEffect(() => {
    const editor = editorRef.current;
    if (!editor) return;
    editor.session.setMode(`ace/mode/${aceModeName(mode)}`);
  }, [mode]);

  useEffect(() => {
    const editor = editorRef.current;
    if (!editor) return;
    editor.setReadOnly(!!disabled);
  }, [disabled]);

  useEffect(() => {
    const editor = editorRef.current;
    if (!editor) return;
    editor.setTheme(aceThemePath(theme));
  }, [theme]);

  return <div className="code-editor-host" ref={hostRef}></div>;
}

function App() {
  // Auth is now cookie-based (HttpOnly opanel_ent_session). The SPA does not see
  // the JWT at all. We track only whether the user is authenticated in memory.
  const [isAuthenticated, setIsAuthenticated] = useState(false);
  const [currentUser, setCurrentUser] = useState(null);
  const [bootstrapping, setBootstrapping] = useState(true);
  const [standaloneEditor] = useState(() => editorParamsFromLocation());
  const [theme, setTheme] = useState(() => {
    const saved = localStorage.getItem('opanel-ent-theme');
    if (saved === 'dark' || saved === 'light') return saved;
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  });
  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
    localStorage.setItem('opanel-ent-theme', theme);
  }, [theme]);
  const toggleTheme = () => setTheme(prev => prev === 'dark' ? 'light' : 'dark');
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [otpCode, setOtpCode] = useState('');
  const [needsTwoFactor, setNeedsTwoFactor] = useState(false);
  const [page, setPage] = useState(() => pageFromPathname(window.location.pathname));
  const [domain, setDomain] = useState('');
  const [websiteSearch, setWebsiteSearch] = useState('');
  const [databaseSearch, setDatabaseSearch] = useState('');
  const [adminEmail, setAdminEmail] = useState('');
  const [wpAdminUser, setWpAdminUser] = useState('admin');
  const [wpAdminPassword, setWpAdminPassword] = useState('');
  const [phpVersion, setPhpVersion] = useState('8.4');
  const [siteType, setSiteType] = useState('wordpress');
  const [installSslAfterCreate, setInstallSslAfterCreate] = useState(false);
  const [installWordPress, setInstallWordPress] = useState(true);
  const [nginxCustomEditing, setNginxCustomEditing] = useState(null); // Website settings editor state
  const [websiteSettingsForm, setWebsiteSettingsForm] = useState(websiteConfigForm());
  const [logViewer, setLogViewer] = useState(null); // {id, domain, kind, lines, path, content, exists}
  const [terminalViewer, setTerminalViewer] = useState(null); // {id, domain}
  const [websites, setWebsites] = useState([]);
  const [aliasDrafts, setAliasDrafts] = useState({});
  const [aliasModes, setAliasModes] = useState({});
  const [databases, setDatabases] = useState([]);
  const [newDatabase, setNewDatabase] = useState({ db_name: '', db_user: '', db_password: '' });
  const [createdDbInfo, setCreatedDbInfo] = useState(null);
  const [copiedField, setCopiedField] = useState(null);
  const [users, setUsers] = useState([]);
  const [resourceUsage, setResourceUsage] = useState(null);
  const [serviceStates, setServiceStates] = useState({});
  const [serviceNames, setServiceNames] = useState(DEFAULT_SERVICE_NAMES);
  const [backupTab, setBackupTab] = useState('website');
  const [backups, setBackups] = useState([]);
  const [backupJobs, setBackupJobs] = useState([]);
  const [userBackups, setUserBackups] = useState([]);
  const [restoreBackups, setRestoreBackups] = useState([]);
  const [restoreBackupDir, setRestoreBackupDir] = useState('');
  const [selectedBackupUserId, setSelectedBackupUserId] = useState('');
  const [backupSchedules, setBackupSchedules] = useState([]);
  const [newBackupSchedule, setNewBackupSchedule] = useState({ user_ids: [], all_users: false, schedule: '0 2 * * *', target_id: '', retention: 7 });
  const [sftpTargets, setSftpTargets] = useState([]);
  const [selectedSftpTargetId, setSelectedSftpTargetId] = useState('');
  const [newSftpTarget, setNewSftpTarget] = useState({ name: '', host: '', port: 22, username: '', password: '', private_key: '', remote_path: '/backups/opanel-ent' });
  const [daBackups, setDaBackups] = useState([]);
  const [daBackupDir, setDaBackupDir] = useState('');
  const [daImportJobs, setDaImportJobs] = useState([]);
  const [selectedWebsiteId, setSelectedWebsiteId] = useState(() => standaloneEditor?.websiteId || '');
  const [sslMode, setSslMode] = useState('letsencrypt');
  const [manualSslForm, setManualSslForm] = useState({ certificate: '', private_key: '', ca_bundle: '' });
  const [manualSslFiles, setManualSslFiles] = useState({ certificate: null, private_key: null, ca_bundle: null });
  const [wildcardSslForm, setWildcardSslForm] = useState({ api_token: '', email: '' });
  const [availableCerts, setAvailableCerts] = useState([]);   // SSL page: certs on the box covering currentSite
  const [reuseCertName, setReuseCertName] = useState('');
  // Create-website SSL section
  const [createSslMode, setCreateSslMode] = useState('letsencrypt'); // letsencrypt|wildcard|existing|manual
  const [createSslForm, setCreateSslForm] = useState({ api_token: '', email: '', reuse_name: '', certificate: '', private_key: '', ca_bundle: '' });
  const [createSslCerts, setCreateSslCerts] = useState([]);
  const [cronSchedule, setCronSchedule] = useState('*/15 * * * *');
  const [cronCommand, setCronCommand] = useState('');
  const [cronItems, setCronItems] = useState([]);
  const [cronUser, setCronUser] = useState('');
  const [filePath, setFilePath] = useState(() => standaloneEditor?.path || 'public_html/index.html');
  const [fileListPath, setFileListPath] = useState('public_html');
  const [fileUploadDir, setFileUploadDir] = useState('public_html');
  const [files, setFiles] = useState([]);
  const [fileJobs, setFileJobs] = useState([]);
  const [fileContent, setFileContent] = useState('');
  const [selectedFilePaths, setSelectedFilePaths] = useState([]);
  const [archiveFormat, setArchiveFormat] = useState('zip');
  const [editorCursor, setEditorCursor] = useState({ line: 1, column: 1 });
  const [newUser, setNewUser] = useState({ username: '', email: '', password: '', role: 'end_user', website_limit: 5, storage_limit_mb: 1024 });
  const [editingUser, setEditingUser] = useState(null);
  const [editingUserForm, setEditingUserForm] = useState({ email: '', role: 'end_user', website_limit: 5, storage_limit_mb: 1024 });
  const [phpConfig, setPhpConfig] = useState({ php_version: '8.4', display_errors: 'Off', max_execution_time: 300, max_input_time: 600, max_input_vars: 10000, memory_limit: '1024M', post_max_size: '1024M', upload_max_filesize: '1024M', opcache_enable: true });
  const [phpVersions, setPhpVersions] = useState({ installed: [], supported: [] });
  const [phpTuning, setPhpTuning] = useState(null);
  const [firewallStatus, setFirewallStatus] = useState(null);
  const [firewallPort, setFirewallPort] = useState('80');
  const [firewallProtocol, setFirewallProtocol] = useState('tcp');
  const [firewallAllowIp, setFirewallAllowIp] = useState('');
  const [firewallAllowPort, setFirewallAllowPort] = useState('');
  const [firewallAllowProtocol, setFirewallAllowProtocol] = useState('tcp');
  const [firewallBlockIp, setFirewallBlockIp] = useState('');
  const [firewallBlockPort, setFirewallBlockPort] = useState('');
  const [firewallBlockProtocol, setFirewallBlockProtocol] = useState('tcp');
  const [firewallDeleteNumber, setFirewallDeleteNumber] = useState('');
  const [firewallBlocklists, setFirewallBlocklists] = useState(null);
  const [firewallBlocklistUrl, setFirewallBlocklistUrl] = useState('');
  const [wafRules, setWafRules] = useState({ status: null, default_rules: '', custom_rules: '' });
  const [badBots, setBadBots] = useState({ patterns: [], max_patterns: 400 });
  const [badBotText, setBadBotText] = useState('');
  const [wafBotExtra, setWafBotExtra] = useState('');
  const [wafCustomRules, setWafCustomRules] = useState('');
  const [selectedWafWebsiteId, setSelectedWafWebsiteId] = useState('');
  const [wafSiteConfig, setWafSiteConfig] = useState(null);
  const [wafAccessLogs, setWafAccessLogs] = useState({ entries: [], total: 0, domains: [], limit: 50, offset: 0, paths: {} });
  const [wafAccessFilters, setWafAccessFilters] = useState({ domain: '', verdict: '', q: '', limit: 50, offset: 0 });
  const [wafAccessAutoRefresh, setWafAccessAutoRefresh] = useState(5);
  const [assignUserId, setAssignUserId] = useState('');
  const [assignWebsiteId, setAssignWebsiteId] = useState('');
  const [twoFactorStatus, setTwoFactorStatus] = useState(null);
  const [twoFactorSetup, setTwoFactorSetup] = useState(null);
  const [twoFactorCode, setTwoFactorCode] = useState('');
  const [malwareScanStatus, setMalwareScanStatus] = useState(null);
  const [scanTargetWebsiteId, setScanTargetWebsiteId] = useState('');
  const [scanResults, setScanResults] = useState(null);
  const [scanJob, setScanJob] = useState(null);
  const [scanJobs, setScanJobs] = useState([]);
  const [networkStatus, setNetworkStatus] = useState({ ipv4: [], ipv6: [], ipv6_available: false, ipv6_enabled: false });
  const [quarantine, setQuarantine] = useState([]);
  const [malwareSchedules, setMalwareSchedules] = useState({
    system: { scope: 'system', scan_root: '/', enabled: false, frequency: 'weekly', weekday: 0, hour: 2, minute: 0, day: 1 },
    web: { scope: 'web', scan_root: '/home', enabled: false, frequency: 'daily', weekday: 0, hour: 3, minute: 30, day: 1 },
  });
  const [scanLoading, setScanLoading] = useState(false);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  useEffect(() => {
    if (!notice) return undefined;
    const timer = setTimeout(() => setNotice(''), 5000);
    return () => clearTimeout(timer);
  }, [notice]);
  const [loading, setLoading] = useState('');
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);
  const [settingsMenuOpen, setSettingsMenuOpen] = useState(false);
  const [panelSettings, setPanelSettings] = useState({ app_name: 'opanel-ent', panel_url: '', panel_hostname: '', panel_port: 2222, logo_url: '', favicon_url: '/favicon.png', ssl_enabled: false });
  const [panelSettingsForm, setPanelSettingsForm] = useState({ app_name: 'opanel-ent', panel_hostname: '', panel_port: 2222, ssl_enabled: false });
  const [appVersion, setAppVersion] = useState('');
  const [panelLogoFile, setPanelLogoFile] = useState(null);
  const [panelFaviconFile, setPanelFaviconFile] = useState(null);
  const [panelSslEmail, setPanelSslEmail] = useState('');
  const [updatesStatus, setUpdatesStatus] = useState(null);
  const [showUpdateLog, setShowUpdateLog] = useState(false);
  const [osUpdating, setOsUpdating] = useState(false);
  const [panelUpdating, setPanelUpdating] = useState(false);
  const [panelUpdateLog, setPanelUpdateLog] = useState([]);
  const panelUpdateInterval = useRef(null);
  const wafAccessFiltersRef = useRef(wafAccessFilters);
  const [osAutoUpdate, setOsAutoUpdate] = useState({ enabled: true, mode: 'security', auto_reboot: false });
  const [panelAutoUpdate, setPanelAutoUpdate] = useState({ enabled: true, time: '03:30' });
  // API Tokens
  const [apiTokens, setApiTokens] = useState([]);
  const [apiTokenForm, setApiTokenForm] = useState({ name: '', scopes: ['provisioning:read', 'provisioning:write'], expires_days: 365, ip_allowlist: '' });
  const [createdToken, setCreatedToken] = useState(null);
  // Profile modal
  const [showProfileModal, setShowProfileModal] = useState(false);
  const [profileForm, setProfileForm] = useState({ email: '', password: '', current_password: '', code: '' });
  // Users page tabs
  const [usersTab, setUsersTab] = useState('list');
  // Hosting plans
  const [plans, setPlans] = useState([]);
  const [editingPlan, setEditingPlan] = useState(null);
  const [editingPlanForm, setEditingPlanForm] = useState({ name: '', website_limit: 1, storage_limit_mb: 1024, php_version: '8.4', app_type: 'php', auto_ssl: false, active: true });
  const [newPlan, setNewPlan] = useState({ slug: '', name: '', website_limit: 1, storage_limit_mb: 1024, php_version: '8.4', app_type: 'php', auto_ssl: false });
  // WordPress manager
  const [wpManagerSite, setWpManagerSite] = useState(null);
  const [wpManagerMode, setWpManagerMode] = useState('install'); // 'install' | 'update'
  const [wpInstallForm, setWpInstallForm] = useState({ admin_user: 'admin', admin_email: '', admin_password: '', title: '' });
  const noticeTimer = useRef(null);
  const isAdmin = currentUser?.role === 'admin';
  const currentSite = websites.find(site => String(site.id) === String(selectedWebsiteId));

  const navigateToPage = useCallback((nextPage, options = {}) => {
    const route = routeForPage(nextPage);
    if (!route) return;
    const nextUrl = route;
    if (!options.replace && window.location.pathname !== route) {
      window.history.pushState({}, '', nextUrl);
    } else if (options.replace && window.location.pathname !== route) {
      window.history.replaceState({}, '', nextUrl);
    }
    setPage(nextPage);
  }, []);

  // Auto-dismiss notices after 6 seconds
  useEffect(() => {
    if (notice) {
      if (noticeTimer.current) clearTimeout(noticeTimer.current);
      noticeTimer.current = setTimeout(() => setNotice(''), 6000);
    }
    return () => { if (noticeTimer.current) clearTimeout(noticeTimer.current); };
  }, [notice]);

  function readCookie(name) {
    const match = document.cookie.match(new RegExp('(?:^|; )' + name.replace(/[$()*+./?[\\\]^{|}]/g, '\\$&') + '=([^;]*)'));
    return match ? decodeURIComponent(match[1]) : '';
  }

  function clearReadableSessionCookies() {
    try {
      document.cookie = 'opanel_ent_csrf=; Max-Age=0; path=/; SameSite=Lax';
      if (window.location.protocol === 'https:') {
        document.cookie = 'opanel_ent_csrf=; Max-Age=0; path=/; SameSite=Lax; Secure';
      }
    } catch {}
  }

  function currentPanelHost() {
    return window.location.hostname || '';
  }

  function currentPanelPort() {
    const port = Number(window.location.port || 2222);
    return Number.isFinite(port) && port > 0 ? port : 2222;
  }

  function formFromPanelSettings(data = {}) {
    let hostname = data.panel_hostname || currentPanelHost();
    let port = Number(data.panel_port || currentPanelPort());
    if ((!hostname || !port) && data.panel_url) {
      try {
        const parsed = new URL(data.panel_url);
        hostname = hostname || parsed.hostname;
        port = port || Number(parsed.port || 2222);
      } catch {}
    }
    return {
      app_name: data.app_name || 'opanel-ent',
      panel_hostname: hostname,
      panel_port: Number.isFinite(port) && port > 0 ? port : 2222,
      ssl_enabled: !!data.ssl_enabled,
    };
  }

  function clearSession(message = 'Your session expired. Please log in again.') {
    // Old localStorage token from a previous deploy: nuke it for safety.
    try { localStorage.removeItem('token'); } catch {}
    clearReadableSessionCookies();
    setIsAuthenticated(false);
    setCurrentUser(null);
    setNeedsTwoFactor(false);
    setOtpCode('');
    setWebsites([]);
    setDatabases([]);
    setUsers([]);
    setResourceUsage(null);
    setServiceStates({});
    setServiceNames(DEFAULT_SERVICE_NAMES);
    setBackupTab('website');
    setBackups([]);
    setBackupJobs([]);
    setCronItems([]);
    setCronUser('');
    setUserBackups([]);
    setRestoreBackups([]);
    setRestoreBackupDir('');
    setSelectedBackupUserId('');
    setBackupSchedules([]);
    setSftpTargets([]);
    setSelectedSftpTargetId('');
    setTwoFactorStatus(null);
    setTwoFactorSetup(null);
    setTwoFactorCode('');
    setMalwareScanStatus(null);
    setScanTargetWebsiteId('');
    setScanResults(null);
    setScanJob(null);
    setScanJobs([]);
    setScanLoading(false);
    setUpdatesStatus(null);
    setFirewallBlocklists(null);
    setWafRules({ status: null, default_rules: '', custom_rules: '' });
    setWafCustomRules('');
    setSelectedWafWebsiteId('');
    setWafSiteConfig(null);
    setWafAccessLogs({ entries: [], total: 0, domains: [], limit: 50, offset: 0, paths: {} });
    setWafAccessFilters({ domain: '', verdict: '', q: '', limit: 50, offset: 0 });
    setWafAccessAutoRefresh(5);
    setLogViewer(null);
    setNginxCustomEditing(null);
    setTerminalViewer(null);
    setSelectedWebsiteId('');
    setMobileMenuOpen(false);
    navigateToPage('dashboard', { replace: true });
    setError('');
    setNotice(message);
  }

  function handleAuthExpired(status, detail = '') {
    if (status === 401 || detail === 'Could not validate credentials' || detail === 'Not authenticated') {
      clearSession();
      return true;
    }
    return false;
  }

  async function request(path, options = {}, label = '') {
    try {
      setError('');
      if (label) setLoading(label);
      const { silent, ...fetchOptions } = options;
      const method = (fetchOptions.method || 'GET').toUpperCase();
      const isFormData = typeof FormData !== 'undefined' && fetchOptions.body instanceof FormData;
      const headers = isFormData ? { ...(fetchOptions.headers || {}) } : {
        'Content-Type': 'application/json',
        ...(fetchOptions.headers || {}),
      };
      // CSRF: echo the opanel_ent_csrf cookie back in a header for mutating
      // requests. The backend rejects mismatches when the request was
      // authenticated via cookie.
      if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(method)) {
        const csrf = readCookie('opanel_ent_csrf');
        if (csrf) headers['X-CSRF-Token'] = csrf;
      }
      const res = await fetch(`${API}${path}`, {
        ...fetchOptions,
        credentials: 'include',
        headers,
      });
      const text = await res.text();
      let data;
      try { data = text ? JSON.parse(text) : {}; } catch { data = { detail: text || `HTTP ${res.status}` }; }
      if (!res.ok && handleAuthExpired(res.status, data.detail)) return null;
      if (!res.ok && !silent) setError(formatApiError(data.detail, `Request failed with status ${res.status}`));
      if (res.ok && data?.message && !silent) setNotice(data.message);
      return res.ok ? data : null;
    } catch (err) {
      setError(`Cannot connect to the ${panelSettings.app_name || 'opanel-ent'} API at ${API}. Check opanel-ent-api and the panel port.`);
      return null;
    } finally {
      if (label) setLoading('');
    }
  }

  async function login() {
    try {
      setError('');
      setLoading('Logging in...');
      const body = new URLSearchParams({ username, password });
      if (needsTwoFactor || otpCode) body.set('otp', otpCode);
      const res = await fetch(`${API}/auth/login`, {
        method: 'POST',
        body,
        credentials: 'include',
      });
      const data = await res.json().catch(() => ({}));
      if (res.ok && data.requires_2fa) {
        setNeedsTwoFactor(true);
        setNotice('Enter your authentication code.');
      } else if (res.ok && data.access_token) {
        // Don't keep the token anywhere: the HttpOnly cookie just got set by
        // the response. JS code MUST NOT touch the JWT.
        setIsAuthenticated(true);
        setNeedsTwoFactor(false);
        setOtpCode('');
        setNotice('Login successful.');
        await loadCurrentUser();
      } else {
        setError(formatApiError(data.detail, `Login failed with status ${res.status}`));
      }
    } catch (err) {
      setError(`Cannot connect to the ${panelSettings.app_name || 'opanel-ent'} API at ${API}. Check opanel-ent-api and the panel port.`);
    } finally {
      setLoading('');
    }
  }

  async function logout() {
    try {
      // Best-effort server logout: clears cookies and bumps token_version.
      await fetch(`${API}/auth/logout`, {
        method: 'POST',
        credentials: 'include',
        headers: (() => {
          const csrf = readCookie('opanel_ent_csrf');
          return csrf ? { 'X-CSRF-Token': csrf } : {};
        })(),
      });
    } catch {}
    clearSession('Logged out.');
  }

  async function loadCurrentUser({ clearOnUnauthorized = true } = {}) {
    try {
      const res = await fetch(`${API}/auth/session`, { credentials: 'include' });
      if (!res.ok) {
        if (res.status === 401) {
          if (clearOnUnauthorized) clearSession('Session expired.');
          else {
            clearReadableSessionCookies();
            setCurrentUser(null);
            setIsAuthenticated(false);
          }
        }
        return null;
      }
      const data = await res.json();
      if (!data.authenticated || !data.user) {
        if (clearOnUnauthorized) clearSession('Session expired.');
        else {
          clearReadableSessionCookies();
          setCurrentUser(null);
          setIsAuthenticated(false);
        }
        return null;
      }
      setCurrentUser(data.user);
      setIsAuthenticated(true);
      return data.user;
    } catch {
      setCurrentUser(null);
      return null;
    }
  }

  async function loadPanelSettings() {
    try {
      const res = await fetch(`${API}/panel-settings/public`, { credentials: 'include' });
      if (!res.ok) return null;
      const data = await res.json();
      setPanelSettings(data);
      setPanelSettingsForm(formFromPanelSettings(data));
      return data;
    } catch {
      return null;
    }
  }

  async function loadAppVersion() {
    try {
      const res = await fetch(`${API}/health`, { credentials: 'include' });
      if (!res.ok) return;
      const data = await res.json();
      setAppVersion(data.version || '');
    } catch {}
  }

  async function savePanelSettings() {
    const wantsSsl = !!panelSettingsForm.ssl_enabled;
    const hasSsl = !!panelSettings.ssl_enabled;
    const hostname = String(panelSettingsForm.panel_hostname || '').trim();
    const port = Number(panelSettingsForm.panel_port || 2222);
    const currentHostname = panelSettings.panel_hostname || currentPanelHost();
    const hostnameChanged = hostname && hostname !== currentHostname;

    if (wantsSsl && (!hasSsl || hostnameChanged)) {
      const sslEmail = String(currentUser?.email || panelSslEmail || defaultPanelSslEmail(hostname) || '').trim();
      const nameData = await request('/panel-settings', {
        method: 'PATCH',
        body: JSON.stringify({ app_name: panelSettingsForm.app_name }),
      }, 'Saving panel settings...');
      if (!nameData) return;
      const sslData = await request('/panel-settings/ssl', {
        method: 'POST',
        body: JSON.stringify({ panel_hostname: hostname, panel_port: port, ...(sslEmail ? { email: sslEmail } : {}) }),
      }, 'Installing panel SSL...');
      if (sslData) {
        setPanelSettings(sslData);
        setPanelSettingsForm(formFromPanelSettings(sslData));
        setNotice(sslData.message || 'Panel SSL installed. The panel may restart in a moment.');
      }
      return;
    }

    const payload = hasSsl && !wantsSsl
      ? { app_name: panelSettingsForm.app_name, panel_url: `http://${hostname}:${port}` }
      : { app_name: panelSettingsForm.app_name, panel_hostname: hostname };
    const data = await request('/panel-settings', {
      method: 'PATCH',
      body: JSON.stringify(payload),
    }, 'Saving panel settings...');
    if (data) {
      setPanelSettings(data);
      setPanelSettingsForm(formFromPanelSettings(data));
      setNotice(hasSsl && !wantsSsl ? 'Panel SSL disabled. The panel remains reachable by IP and port over HTTP.' : 'Panel settings updated.');
    }
  }

  async function uploadPanelAsset(kind) {
    const file = kind === 'logo' ? panelLogoFile : panelFaviconFile;
    if (!file) return;
    const body = new FormData();
    body.append('file', file);
    const data = await request(`/panel-settings/${kind}`, { method: 'POST', body }, `Uploading ${kind}...`);
    if (data) {
      setPanelSettings(data);
      setPanelSettingsForm(formFromPanelSettings(data));
      if (kind === 'logo') setPanelLogoFile(null);
      if (kind === 'favicon') setPanelFaviconFile(null);
    }
  }

  function brandInitials(value = panelSettings.app_name) {
    const words = String(value || 'opanel-ent').trim().split(/\s+/).filter(Boolean);
    const initials = words.length > 1 ? `${words[0][0]}${words[1][0]}` : words[0]?.slice(0, 2);
    return (initials || 'BP').toUpperCase();
  }

  function renderBrandMark(extraClass = '') {
    const classes = ['brand-mark', panelSettings.logo_url ? 'has-logo' : '', extraClass].filter(Boolean).join(' ');
    return <span className={classes}>{panelSettings.logo_url ? <img src={panelSettings.logo_url} alt="" /> : brandInitials()}</span>;
  }

  // Bootstrap: ask for session state without turning an anonymous visit into
  // a console-level 401.
  useEffect(() => {
    (async () => {
      try {
        await loadCurrentUser({ clearOnUnauthorized: false });
      } catch {}
      finally { setBootstrapping(false); }
    })();
  }, []);

  useEffect(() => { loadPanelSettings(); loadAppVersion(); }, []);

  useEffect(() => {
    const appName = panelSettings.app_name || 'opanel-ent';
    document.title = appName;
    const configuredFaviconUrl = panelSettings.favicon_url || '/favicon.png';
    const faviconUrl = configuredFaviconUrl.includes('?')
      ? configuredFaviconUrl
      : `${configuredFaviconUrl}?v=${encodeURIComponent(appVersion || 'current')}`;
    const pathname = faviconUrl.split('?', 1)[0].toLowerCase();
    const faviconType = pathname.endsWith('.ico') ? 'image/x-icon'
      : pathname.endsWith('.jpg') || pathname.endsWith('.jpeg') ? 'image/jpeg'
        : pathname.endsWith('.webp') ? 'image/webp'
          : 'image/png';
    document.querySelectorAll('link[rel~="icon"]').forEach(link => link.remove());
    const link = document.createElement('link');
    link.rel = 'icon';
    link.type = faviconType;
    link.href = faviconUrl;
    document.head.appendChild(link);
  }, [panelSettings, appVersion]);

  useEffect(() => {
    if (currentUser?.email) setPanelSslEmail(currentUser.email);
  }, [currentUser?.email]);

  async function refreshAll() {
    const [refreshedUser, siteData, dbData] = await Promise.all([
      loadCurrentUser(),
      request('/websites'),
      request('/databases'),
    ]);
    if (siteData) {
      setWebsites(siteData);
      if (!selectedWebsiteId && siteData[0]) setSelectedWebsiteId(String(siteData[0].id));
    }
    if (dbData) setDatabases(dbData);
    loadPhpVersions();
  }

  async function loadUsers() {
    const data = await request('/users');
    if (data) {
      setUsers(data);
      if (!selectedBackupUserId && data[0]) setSelectedBackupUserId(String(data[0].id));
      setNewBackupSchedule(prev => (!prev.all_users && (!prev.user_ids || prev.user_ids.length === 0) && data[0]) ? ({ ...prev, user_ids: [String(data[0].id)] }) : prev);
    }
  }

  async function loadResourceUsage() {
    const data = await request('/services/resource-usage');
    if (data) setResourceUsage(data);
  }

  async function createUser() {
    const data = await request('/users', { method: 'POST', body: JSON.stringify({ ...newUser, website_limit: Number(newUser.website_limit), storage_limit_mb: Number(newUser.storage_limit_mb) }) }, 'Creating user...');
    if (data) {
      setNotice(`Created user ${data.username}`);
      setNewUser({ username: '', email: '', password: '', role: 'end_user', website_limit: 5, storage_limit_mb: 1024 });
      await loadUsers();
    }
  }

  function startEditingUser(user) {
    const matchedPlan = plans.find(p => p.website_limit === user.website_limit && p.storage_limit_mb === user.storage_limit_mb);
    setEditingUser(user);
    setEditingUserForm({
      email: user.email || '',
      role: user.role || 'end_user',
      website_limit: user.website_limit ?? 5,
      storage_limit_mb: user.storage_limit_mb ?? 1024,
      _password: '',
      _planId: matchedPlan ? String(matchedPlan.id) : '',
    });
  }

  function cancelEditingUser() {
    setEditingUser(null);
    setEditingUserForm({ email: '', role: 'end_user', website_limit: 5, storage_limit_mb: 1024 });
  }

  async function updatePanelUser() {
    if (!editingUser) return;
    const websiteLimit = Number(editingUserForm.website_limit);
    const storageLimitMb = Number(editingUserForm.storage_limit_mb);
    if (!editingUserForm.email.trim()) { setError('Email is required.'); return; }
    if (!Number.isInteger(websiteLimit) || websiteLimit < 0 || websiteLimit > 1000) {
      setError('Website limit must be between 0 and 1000.');
      return;
    }
    if (!Number.isInteger(storageLimitMb) || storageLimitMb < 0 || storageLimitMb > 1024 * 1024) {
      setError('Storage limit must be between 0 and 1048576 MB.');
      return;
    }
    const payload = {
      email: editingUserForm.email.trim(),
      website_limit: websiteLimit,
      storage_limit_mb: storageLimitMb,
    };
    if (editingUser.id !== currentUser?.id) payload.role = editingUserForm.role;
    const data = await request(`/users/${editingUser.id}`, {
      method: 'PATCH',
      body: JSON.stringify(payload),
    }, `Updating ${editingUser.username}...`);
    if (data) {
      // Change password if provided
      if (editingUserForm._password && editingUserForm._password.length >= 12) {
        const pwPayload = { password: editingUserForm._password };
        if (editingUser.id === currentUser?.id) {
          const currentPassword = prompt('Enter your current password to confirm:');
          if (!currentPassword) { setNotice('User updated but password was not changed.'); cancelEditingUser(); await loadUsers(); return; }
          pwPayload.current_password = currentPassword;
          if (currentUser?.totp_enabled) {
            const code = prompt('Enter your 2FA code:');
            if (code) pwPayload.code = code.trim();
          }
        }
        await request(`/users/${editingUser.id}/password`, { method: 'POST', body: JSON.stringify(pwPayload) }, `Changing password for ${editingUser.username}...`);
      }
      setNotice(`Updated user ${data.username}.`);
      if (data.id === currentUser?.id) setCurrentUser(prev => ({ ...prev, ...data }));
      cancelEditingUser();
      await loadUsers();
    }
  }

  async function changeUserPassword(user) {
    const password = prompt(`Enter a new password for ${user.username} (minimum 12 characters):`);
    if (!password) return;
    if (password.length < 12) { setError('Password must be at least 12 characters.'); return; }
    const payload = { password };
    if (user.id === currentUser?.id) {
      const currentPassword = prompt('Enter your current password to confirm this change:');
      if (!currentPassword) return;
      payload.current_password = currentPassword;
      if (currentUser?.totp_enabled) {
        const code = prompt('Enter the 6-digit code from your authenticator:');
        if (!code) return;
        payload.code = code.trim();
      }
    }
    const data = await request(`/users/${user.id}/password`, { method: 'POST', body: JSON.stringify(payload) }, `Changing password for ${user.username}...`);
    if (data?.message) setNotice(data.message);
  }

  async function deletePanelUser(user) {
    if (!user || user.id === currentUser?.id) return;
    if (!confirm(`Delete panel user ${user.username} and permanently delete all owned websites, files, databases, and Linux user data?`)) return;
    const data = await request(`/users/${user.id}`, { method: 'DELETE' }, `Deleting user ${user.username}...`);
    if (data) {
      const count = data.deleted_websites?.length || 0;
      setNotice(`Deleted user ${user.username}${count ? ` and ${count} website(s)` : ''}`);
      await loadUsers();
      await loadWebsites();
    }
  }

  async function toggleUserActive(user) {
    if (!user) return;
    const action = user.is_active ? 'suspend' : 'unsuspend';
    if (!confirm(`${action === 'suspend' ? 'Suspend' : 'Unsuspend'} ${user.username}?`)) return;
    try {
      await request(`/users/${user.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ is_active: !user.is_active }),
      }, `${action === 'suspend' ? 'Suspending' : 'Unsuspending'} ${user.username}...`);
      setNotice(`${user.username} ${action === 'suspend' ? 'suspended' : 'unsuspended'}.`);
      await loadUsers();
    } catch (err) {
      setError(`Failed to ${action} ${user.username}: ${err?.message || err}`);
    }
  }

  async function quickLoginUser(user) {
    if (!user) return;
    if (!confirm(`Login as ${user.username}? New websites will belong to this user.`)) return;
    // Impersonation re-prompts TOTP when the calling admin has 2FA enabled.
    // Try without the code first; if the backend says one is required, ask
    // and resend. Sending the OTP via FormData keeps it out of the URL.
    let body;
    if (currentUser?.totp_enabled) {
      const code = prompt(`Enter the 6-digit code from your authenticator to confirm impersonation of ${user.username}:`);
      if (!code) return;
      body = new URLSearchParams({ otp: code.trim() });
    }
    const data = await request(
      `/auth/impersonate/${user.id}`,
      body
        ? { method: 'POST', body, headers: { 'Content-Type': 'application/x-www-form-urlencoded' } }
        : { method: 'POST' },
      `Logging in as ${user.username}...`,
    );
    // Handle case where backend says 2FA is required (e.g., stale user object).
    if (data?.requires_2fa) {
      const code = prompt(`Enter the 6-digit code from your authenticator to confirm impersonation of ${user.username}:`);
      if (!code) return;
      const retryBody = new URLSearchParams({ otp: code.trim() });
      const retryData = await request(
        `/auth/impersonate/${user.id}`,
        { method: 'POST', body: retryBody, headers: { 'Content-Type': 'application/x-www-form-urlencoded' } },
        `Logging in as ${user.username}...`,
      );
      if (retryData?.access_token) {
        setNotice(`Logged in as ${user.username}.`);
        await loadCurrentUser();
        navigateToPage('websites');
        await refreshAll();
      }
      return;
    }
    if (data?.access_token) {
      setNotice(`Logged in as ${user.username}.`);
      await loadCurrentUser();
      navigateToPage('websites');
      await refreshAll();
    }
  }

  async function changeMyPassword() { if (!currentUser) return; await changeUserPassword(currentUser); }

  // --- API Tokens ---
  async function loadApiTokens() {
    const data = await request('/api-tokens');
    if (Array.isArray(data)) setApiTokens(data);
  }

  async function createApiToken() {
    if (!apiTokenForm.name.trim()) return;
    setCreatedToken(null);
    const data = await request('/api-tokens', { method: 'POST', body: JSON.stringify(apiTokenForm) }, 'Creating API token...');
    if (data?.token) {
      setCreatedToken(data);
      setApiTokenForm({ name: '', scopes: ['provisioning:read', 'provisioning:write'], expires_days: 365, ip_allowlist: '' });
      loadApiTokens();
    }
  }

  async function deleteApiToken(token) {
    if (!confirm(`Delete token "${token.name}"? This cannot be undone.`)) return;
    const data = await request(`/api-tokens/${token.id}`, { method: 'DELETE' }, 'Deleting token...');
    if (data?.ok) { setNotice('Token deleted.'); loadApiTokens(); }
  }

  // --- Profile ---
  async function updateMyEmail() {
    if (!profileForm.email.trim()) return;
    const data = await request('/users/me', { method: 'PATCH', body: JSON.stringify({ email: profileForm.email }) }, 'Updating email...');
    if (data?.email) {
      setCurrentUser(data);
      setNotice('Email updated.');
      setProfileForm(prev => ({ ...prev, email: data.email }));
    }
  }

  async function changeMyPasswordFromProfile() {
    if (!profileForm.password || profileForm.password.length < 12) { setError('Password must be at least 12 characters.'); return; }
    const payload = { password: profileForm.password };
    if (profileForm.current_password) payload.current_password = profileForm.current_password;
    if (profileForm.code) payload.code = profileForm.code;
    const data = await request(`/users/${currentUser.id}/password`, { method: 'POST', body: JSON.stringify(payload) }, 'Changing password...');
    if (data?.message) {
      setNotice(data.message);
      setProfileForm(prev => ({ ...prev, password: '', current_password: '', code: '' }));
    }
  }

  function openProfileModal() {
    setProfileForm({ email: currentUser?.email || '', password: '', current_password: '', code: '' });
    setShowProfileModal(true);
  }

  // --- Hosting Plans ---
  async function loadPlans() {
    const data = await request('/plans');
    if (Array.isArray(data)) setPlans(data);
  }

  // --- WordPress Install on existing site ---
  function openWpInstall(site) {
    setWpManagerSite(site);
    setWpManagerMode('install');
    setWpInstallForm({ admin_user: 'admin', admin_email: currentUser?.email || '', admin_password: '', title: site.domain });
  }

  function openWpUpdate(site) {
    setWpManagerSite(site);
    setWpManagerMode('update');
  }

  async function installWpOnSite() {
    if (!wpManagerSite || wpManagerMode !== 'install') return;
    if (!wpInstallForm.admin_password || wpInstallForm.admin_password.length < 12) { setError('WP admin password must be at least 12 characters.'); return; }
    const data = await request(`/maintenance/wordpress/${wpManagerSite.id}/install`, {
      method: 'POST', body: JSON.stringify(wpInstallForm),
    }, `Installing WordPress on ${wpManagerSite.domain}...`);
    if (data?.message) {
      setNotice(`${data.message}\nAdmin: ${data.admin_user} | Password: ${wpInstallForm.admin_password}`);
      setWpManagerSite(null);
      refreshAll();
    }
  }

  async function updateWpOnSite() {
    if (!wpManagerSite || wpManagerMode !== 'update') return;
    const data = await request(`/maintenance/wordpress/${wpManagerSite.id}/update-all`, {
      method: 'POST',
    }, `Updating WordPress on ${wpManagerSite.domain}...`);
    if (data?.message) {
      setNotice(data.message);
      setWpManagerSite(null);
    }
  }

  async function createPlan() {
    if (!newPlan.slug.trim() || !newPlan.name.trim()) return;
    const data = await request('/plans', { method: 'POST', body: JSON.stringify(newPlan) }, 'Creating plan...');
    if (data?.id) {
      setNotice(`Plan "${data.name}" created.`);
      setNewPlan({ slug: '', name: '', website_limit: 1, storage_limit_mb: 1024, php_version: '8.4', app_type: 'php', auto_ssl: false });
      loadPlans();
    }
  }

  function startEditingPlan(plan) {
    setEditingPlan(plan);
    setEditingPlanForm({ name: plan.name, website_limit: plan.website_limit, storage_limit_mb: plan.storage_limit_mb, php_version: plan.php_version, app_type: plan.app_type, auto_ssl: plan.auto_ssl, active: plan.active });
  }

  async function updatePlan() {
    if (!editingPlan) return;
    const data = await request(`/plans/${editingPlan.id}`, { method: 'PATCH', body: JSON.stringify(editingPlanForm) }, 'Updating plan...');
    if (data?.id) { setNotice('Plan updated.'); setEditingPlan(null); loadPlans(); }
  }

  async function deletePlanItem(plan) {
    if (!confirm(`Delete plan "${plan.name}"?`)) return;
    const data = await request(`/plans/${plan.id}`, { method: 'DELETE' }, 'Deleting plan...');
    if (data?.ok) { setNotice('Plan deleted.'); setEditingPlan(null); loadPlans(); }
  }

  async function loadTwoFactorStatus() {
    const data = await request('/auth/2fa/status');
    if (data) setTwoFactorStatus(data);
  }

  async function setupTwoFactorAuth() {
    const currentPassword = prompt('Enter your current password to generate a new 2FA secret:');
    if (!currentPassword) return;
    const payload = { current_password: currentPassword };
    if (currentUser?.totp_enabled) {
      const code = prompt('Enter the 6-digit code from your authenticator:');
      if (!code) return;
      payload.code = code.trim();
    }
    const data = await request('/auth/2fa/setup', { method: 'POST', body: JSON.stringify(payload) }, 'Preparing 2FA...');
    if (data) {
      setTwoFactorSetup(data);
      setTwoFactorStatus({ enabled: false });
    }
  }

  async function enableTwoFactorAuth() {
    const data = await request('/auth/2fa/enable', { method: 'POST', body: JSON.stringify({ code: twoFactorCode }) }, 'Enabling 2FA...');
    if (data) {
      setTwoFactorStatus(data);
      setTwoFactorSetup(null);
      setTwoFactorCode('');
      await loadCurrentUser();
      setNotice('2FA enabled.');
    }
  }

  async function disableTwoFactorAuth() {
    const currentPassword = prompt('Enter your current password to disable 2FA:');
    if (!currentPassword) return;
    const data = await request(
      '/auth/2fa/disable',
      { method: 'POST', body: JSON.stringify({ current_password: currentPassword, code: twoFactorCode }) },
      'Disabling 2FA...',
    );
    if (data) {
      setTwoFactorStatus(data);
      setTwoFactorCode('');
      await loadCurrentUser();
      setNotice('2FA disabled.');
    }
  }

  async function resetUserTwoFactor(user) {
    if (!confirm(`Reset 2FA for ${user.username}?`)) return;
    const data = await request(`/users/${user.id}/2fa/reset`, { method: 'POST' }, `Resetting 2FA for ${user.username}...`);
    if (data?.message) { setNotice(data.message); await loadUsers(); }
  }

  async function loadNetworkStatus() {
    const data = await request('/panel-settings/network', { silent: true }, '');
    if (data) setNetworkStatus(data);
    return data;
  }

  async function toggleIpv6(enable) {
    const data = await request('/panel-settings/network/ipv6', {
      method: 'POST',
      body: JSON.stringify({ enabled: enable }),
    }, enable ? 'Enabling IPv6...' : 'Disabling IPv6...');
    if (data) {
      setNetworkStatus(data);
      setNotice(data.message || `IPv6 ${enable ? 'enabled' : 'disabled'}.`);
    }
  }

  async function loadMalwareScanStatus() {
    const data = await request('/panel-settings/malware-scan', {}, 'Loading malware scan status...');
    if (data) setMalwareScanStatus(data);
  }

  async function toggleMalwareScan(enable) {
    if (enable && !malwareScanStatus?.installed) {
      if (!confirm('ClamAV is not installed on this server. It will be installed now (may take 1-2 minutes). Continue?')) return;
    }
    const data = await request('/panel-settings/malware-scan/toggle', {
      method: 'POST',
      body: JSON.stringify({ enabled: enable }),
    }, enable ? 'Enabling malware scanning...' : 'Disabling malware scanning...');
    if (data) {
      setPanelSettings(data);
      setNotice(data.message || `Malware scanning ${enable ? 'enabled' : 'disabled'}.`);
      await loadMalwareScanStatus();
    }
  }

  async function toggleMalwareRealtime(enable) {
    if (enable && !confirm(
      'Turn on real-time protection?\n\n'
      + '• Linux Malware Detect watches every file under /home (inotify) and scans new or changed files within seconds.\n'
      + '• It uses more RAM the more files it watches — heavy on a box with many WordPress sites.\n'
      + '• Hits are NOT auto-quarantined — they are listed for you to act on.\n\n'
      + 'Turning it off returns to scheduled scans only.'
    )) return;
    const data = await request('/panel-settings/malware-scan/realtime', {
      method: 'POST',
      body: JSON.stringify({ enabled: enable }),
    }, enable ? 'Enabling real-time protection...' : 'Disabling real-time protection...');
    if (data) {
      setPanelSettings(data);
      setNotice(data.message || `Real-time protection ${enable ? 'enabled' : 'disabled'}.`);
      await loadMalwareScanStatus();
    }
  }

  async function updateMalwareSignatures() {
    const data = await request('/panel-settings/malware-scan/update-signatures', { method: 'POST' }, 'Updating malware signatures...');
    if (data) { setNotice(data.detail || 'Signatures updated.'); await loadMalwareScanStatus(); }
  }

  async function toggleAutoQuarantine(enable) {
    const data = await request('/panel-settings/malware-scan/auto-quarantine', {
      method: 'POST',
      body: JSON.stringify({ enabled: enable }),
    }, 'Saving...');
    if (data) { setPanelSettings(data); setNotice(data.message || ''); await loadMalwareScanStatus(); }
  }

  async function loadQuarantine() {
    const data = await request('/panel-settings/malware-scan/quarantine', { silent: true }, '');
    if (data && Array.isArray(data.entries)) setQuarantine(data.entries);
  }

  async function quarantineThreat(path, signature) {
    if (!confirm(`Move this file out of the site into quarantine?\n\n${path}\n\nIt stops being served immediately. You can restore it from the Quarantine list if it turns out to be a false positive.`)) return;
    const data = await request('/panel-settings/malware-scan/quarantine', {
      method: 'POST',
      body: JSON.stringify({ path, signature: signature || '' }),
    }, 'Quarantining file...');
    if (data && Array.isArray(data.entries)) { setQuarantine(data.entries); setNotice('File moved to quarantine.'); }
  }

  async function restoreQuarantine(id, path) {
    if (!confirm(`Restore this file to its original location?\n\n${path}\n\nOnly do this if you are sure it is a false positive — it will be served again.`)) return;
    const data = await request(`/panel-settings/malware-scan/quarantine/${id}/restore`, { method: 'POST' }, 'Restoring file...');
    if (data && Array.isArray(data.entries)) { setQuarantine(data.entries); setNotice('File restored to its original location.'); }
  }

  async function deleteQuarantine(id, path) {
    if (!confirm(`Permanently delete this quarantined file?\n\n${path}\n\nThis cannot be undone.`)) return;
    const data = await request(`/panel-settings/malware-scan/quarantine/${id}`, { method: 'DELETE' }, 'Deleting quarantined file...');
    if (data && Array.isArray(data.entries)) { setQuarantine(data.entries); setNotice('Quarantined file deleted.'); }
  }

  async function installLmd() {
    if (!confirm('Add Linux Malware Detect to this server?\n\nLMD layers its web-focused signatures on top of ClamAV (catches PHP shells ClamAV misses) and uses the running clamd as its engine. Install runs in the background.')) return;
    const data = await request('/panel-settings/malware-scan/install-lmd', { method: 'POST' }, 'Installing Linux Malware Detect...');
    if (data) { setPanelSettings(data); setNotice(data.message || 'Linux Malware Detect install started.'); await loadMalwareScanStatus(); }
  }

  async function runMalwareScan() {
    if (!scanTargetWebsiteId) return;
    if (scanTargetWebsiteId === 'system'
      && !confirm('Scan the whole server (/) now? The first full scan can run for a long time.')) return;
    setScanResults(null);
    setScanJob(null);
    setScanLoading(true);
    try {
      const body = scanTargetWebsiteId === 'system'
        ? { scope: 'system', scan_root: '/' }
        : scanTargetWebsiteId === 'all'
          ? { all: true }
          : { website_id: Number(scanTargetWebsiteId) };
      const data = await request('/panel-settings/malware-scan/run', {
        method: 'POST',
        body: JSON.stringify(body),
      }, 'Starting malware scan...');
      if (data) {
        setScanJob(data);
        await loadMalwareScanJobs();
        setNotice('Malware scan started.');
      }
    } finally {
      setScanLoading(false);
    }
  }

  async function loadMalwareSchedule() {
    const data = await request('/panel-settings/malware-scan/schedule', { silent: true }, '');
    if (data && data.system && data.web) setMalwareSchedules(data);
    return data;
  }

  async function saveMalwareSchedule(scope) {
    const s = malwareSchedules[scope] || {};
    const data = await request('/panel-settings/malware-scan/schedule', {
      method: 'PUT',
      body: JSON.stringify({
        [scope]: {
          enabled: !!s.enabled,
          frequency: s.frequency || 'weekly',
          hour: Number(s.hour) || 0,
          minute: Number(s.minute) || 0,
          weekday: Number(s.weekday) || 0,
          day: Number(s.day) || 1,
        },
      }),
    }, 'Saving scan schedule...');
    if (data && data.system && data.web) {
      setMalwareSchedules(data);
      setNotice(data[scope]?.enabled ? 'Scheduled scan saved.' : 'Scheduled scan disabled.');
    }
  }

  async function loadMalwareScanJob(jobId) {
    const data = await request(`/panel-settings/malware-scan/jobs/${jobId}`, {}, '');
    if (!data) return null;
    if (['done', 'infected', 'error', 'interrupted'].includes(data.status)) {
      setScanLoading(false);
      setScanJob(null);
      setScanResults(null);
      if (data.status === 'infected' || data.infected > 0) {
        setNotice(`${data.infected} threat(s) found.`);
      } else if (['error', 'interrupted'].includes(data.status)) {
        setError(data.error || data.message || 'Malware scan failed.');
      } else {
        setNotice(`Scan complete: ${data.scanned || 0} files scanned, no threats found.`);
      }
      await loadMalwareScanJobs();
    } else {
      setScanJob(data);
    }
    return data;
  }

  async function loadMalwareScanJobs() {
    const data = await request('/panel-settings/malware-scan/jobs', { silent: true }, '');
    if (data?.jobs) setScanJobs(data.jobs);
    return data?.jobs || [];
  }

  function showMalwareScanJob(job) {
    setScanJob(job);
    setScanResults(job);
    setScanLoading(['queued', 'running'].includes(job?.status));
  }

  async function loadLatestMalwareScanJob() {
    const data = await request('/panel-settings/malware-scan/jobs/latest', { silent: true }, '');
    if (!data) return null;
    if (['done', 'infected', 'error', 'interrupted'].includes(data.status)) {
      setScanJob(null);
      setScanResults(null);
      setScanLoading(false);
    } else {
      setScanJob(data);
    }
    return data;
  }

  async function startClamavDaemon() {
    const data = await request('/panel-settings/malware-scan/start', { method: 'POST' }, 'Starting ClamAV daemon...');
    if (data) {
      setNotice(data.message || 'ClamAV daemon started.');
      await loadMalwareScanStatus();
    }
  }

  async function assignDomainToUser() {
    if (!assignWebsiteId || !assignUserId) return;
    const data = await request(`/websites/${assignWebsiteId}`, { method: 'PATCH', body: JSON.stringify({ owner_id: Number(assignUserId) }) }, 'Assigning domain to user...');
    if (data) { setNotice(`Assigned domain ${data.domain} to user ID ${assignUserId}`); await refreshAll(); }
  }

  async function createWordPress() {
    const cleanDomain = domain.trim().toLowerCase();
    const cleanAdminEmail = adminEmail.trim();
    if (!cleanDomain) { setError('Please enter a domain name.'); return; }
    const installWp = siteType === 'wordpress' && installWordPress;
    const body = {
      domain: cleanDomain,
      php_version: phpVersion,
      app_type: siteType,
      install_wordpress: installWp,
      title: cleanDomain,
    };
    if (installWp) {
      body.admin_user = wpAdminUser;
      body.admin_email = cleanAdminEmail || `admin@${cleanDomain}`;
      body.admin_password = wpAdminPassword || 'StrongPass123!';
    }
    const data = await request('/websites', { method: 'POST', body: JSON.stringify(body) },
      installWp ? 'Creating WordPress website...' : 'Creating website...');
    if (data) {
      if (installWp) {
        setNotice(`Created WordPress site: https://${cleanDomain}\nAdmin: ${wpAdminUser} | Password: ${wpAdminPassword || 'StrongPass123!'}`);
      } else {
        setNotice(`Created site ${cleanDomain}. Upload your files to public_html/ folder.`);
      }
      if (installSslAfterCreate) await applySslForNewSite(data.id, cleanDomain);
      refreshAll();
    }
  }

  async function applySslForNewSite(id, siteDomain) {
    if (createSslMode === 'wildcard') {
      const token = String(createSslForm.api_token || '').trim();
      if (!token) { setError('Enter a Cloudflare API token for wildcard SSL.'); return; }
      const b = { provider: 'cloudflare', api_token: token };
      const em = String(createSslForm.email || '').trim(); if (em) b.email = em;
      await request(`/websites/${id}/ssl/wildcard`, { method: 'POST', body: JSON.stringify(b) }, 'Issuing wildcard certificate...');
    } else if (createSslMode === 'existing') {
      if (!createSslForm.reuse_name) { setError('Pick a certificate to use.'); return; }
      await request(`/websites/${id}/ssl/reuse`, { method: 'POST', body: JSON.stringify({ name: createSslForm.reuse_name }) }, 'Applying existing certificate...');
    } else if (createSslMode === 'manual') {
      const form = new FormData();
      form.append('certificate_text', createSslForm.certificate);
      form.append('private_key_text', createSslForm.private_key);
      if (createSslForm.ca_bundle.trim()) form.append('ca_bundle_text', createSslForm.ca_bundle);
      await request(`/websites/${id}/ssl/manual`, { method: 'POST', body: form }, 'Installing manual SSL...');
    } else {
      await request(`/websites/${id}/ssl`, { method: 'POST' }, "Installing Let's Encrypt SSL...");
    }
  }

  async function loadCreateSslCerts() {
    const cleanDomain = domain.trim().toLowerCase();
    if (!cleanDomain) { setCreateSslCerts([]); return; }
    const data = await request(`/websites/available-certificates?domain=${encodeURIComponent(cleanDomain)}`, { silent: true });
    setCreateSslCerts(Array.isArray(data) ? data : []);
  }

  // Keep the "Use existing" cert list in sync with the domain field.
  useEffect(() => {
    if (!installSslAfterCreate || createSslMode !== 'existing') return;
    const t = setTimeout(loadCreateSslCerts, 400);
    return () => clearTimeout(t);
  }, [domain, createSslMode, installSslAfterCreate]);

  async function deleteWebsite(id) {
    if (!confirm('Delete this website including files, vhost, and database?')) return;
    const data = await request(`/websites/${id}?delete_files=true&delete_database=true`, { method: 'DELETE' }, 'Deleting website...');
    if (data) refreshAll();
  }

  async function enableSsl(id) {
    const data = await request(`/websites/${id}/ssl`, { method: 'POST' }, "Installing Let's Encrypt SSL...");
    if (data) refreshAll();
  }

  async function issueWildcardSsl() {
    if (!selectedWebsiteId) return;
    const token = String(wildcardSslForm.api_token || '').trim();
    if (!token && !currentSite?.ssl_wildcard) { setError('Enter a Cloudflare API token (Zone → DNS → Edit).'); return; }
    const body = { provider: 'cloudflare' };
    if (token) body.api_token = token;
    const email = String(wildcardSslForm.email || '').trim();
    if (email) body.email = email;
    const data = await request(`/websites/${selectedWebsiteId}/ssl/wildcard`, { method: 'POST', body: JSON.stringify(body) }, 'Issuing wildcard certificate via Cloudflare DNS...');
    if (data) { setNotice(`Wildcard certificate issued for ${data.domain} and *.${data.domain}.`); setWildcardSslForm({ api_token: '', email: '' }); refreshAll(); }
  }

  async function loadAvailableCerts() {
    if (!selectedWebsiteId) return;
    const data = await request(`/websites/${selectedWebsiteId}/available-certificates`, {}, 'Loading certificates on this server...');
    const list = Array.isArray(data) ? data : [];
    setAvailableCerts(list);
    const covering = list.find(c => c.covers_domain);
    setReuseCertName(prev => prev || (covering ? covering.name : ''));
  }

  async function reuseExistingSsl() {
    if (!selectedWebsiteId || !reuseCertName) { setError('Pick a certificate to use.'); return; }
    const data = await request(`/websites/${selectedWebsiteId}/ssl/reuse`, { method: 'POST', body: JSON.stringify({ name: reuseCertName }) }, 'Applying certificate...');
    if (data) { setNotice(`${data.domain} now uses ${reuseCertName.split(':')[1]}.`); refreshAll(); }
  }

  async function addWebsiteAlias(site) {
    const cleanAlias = String(aliasDrafts[site.id] || '').trim().toLowerCase();
    const aliasMode = aliasModes[site.id] || 'alias';
    if (!cleanAlias) { setError('Enter a domain.'); return; }
    const data = await request(`/websites/${site.id}/aliases`, {
      method: 'POST',
      body: JSON.stringify({ domain: cleanAlias, mode: aliasMode }),
    }, `Adding ${aliasMode === 'redirect' ? 'redirect' : 'alias'} ${cleanAlias}...`);
    if (data) {
      setNotice(aliasMode === 'redirect'
        ? `Added redirect ${cleanAlias} -> ${site.domain}.`
        : `Added alias ${cleanAlias}. Re-run SSL after DNS points to this server.`);
      setAliasDrafts(prev => ({ ...prev, [site.id]: '' }));
      setNginxCustomEditing(prev => {
        if (!prev || prev.id !== site.id) return prev;
        const nextSite = prev.site || site;
        return { ...prev, site: { ...nextSite, aliases: [...(nextSite.aliases || []), data] } };
      });
      await refreshAll();
    }
  }

  async function deleteWebsiteAlias(site, alias) {
    const label = alias.mode === 'redirect' ? 'redirect' : 'alias';
    if (!confirm(`Remove ${label} ${alias.domain} from ${site.domain}?`)) return;
    const data = await request(`/websites/${site.id}/aliases/${alias.id}`, { method: 'DELETE' }, `Removing ${label} ${alias.domain}...`);
    if (data) {
      setNotice(`Removed ${label} ${alias.domain}.`);
      setNginxCustomEditing(prev => {
        if (!prev || prev.id !== site.id) return prev;
        const nextSite = prev.site || site;
        return { ...prev, site: { ...nextSite, aliases: (nextSite.aliases || []).filter(item => item.id !== alias.id) } };
      });
      await refreshAll();
    }
  }

  async function installManualSsl() {
    if (!selectedWebsiteId) return;
    const hasCert = manualSslFiles.certificate || manualSslForm.certificate.trim();
    const hasKey = manualSslFiles.private_key || manualSslForm.private_key.trim();
    if (!hasCert || !hasKey) {
      setError('Certificate and private key are required.');
      return;
    }
    const form = new FormData();
    if (manualSslFiles.certificate) form.append('certificate', manualSslFiles.certificate);
    else form.append('certificate_text', manualSslForm.certificate);
    if (manualSslFiles.private_key) form.append('private_key', manualSslFiles.private_key);
    else form.append('private_key_text', manualSslForm.private_key);
    if (manualSslFiles.ca_bundle) form.append('ca_bundle', manualSslFiles.ca_bundle);
    else if (manualSslForm.ca_bundle.trim()) form.append('ca_bundle_text', manualSslForm.ca_bundle);
    const data = await request(`/websites/${selectedWebsiteId}/ssl/manual`, { method: 'POST', body: form }, 'Installing manual SSL...');
    if (data) {
      setManualSslForm({ certificate: '', private_key: '', ca_bundle: '' });
      setManualSslFiles({ certificate: null, private_key: null, ca_bundle: null });
      refreshAll();
    }
  }

  async function openNginxCustom(site) {
    setLogViewer(null);
    setTerminalViewer(null);
    setWebsiteSettingsForm(websiteConfigForm(site));
    setNginxCustomEditing({
      id: site.id,
      domain: site.domain,
      site,
      mode: 'settings',
      content: '',
    });
  }

  async function viewFullNginxConfig() {
    if (!nginxCustomEditing) return;
    const data = await request(`/websites/${nginxCustomEditing.id}/nginx-config`, {}, 'Loading VHost Config...');
    if (data !== null) {
      setNginxCustomEditing(prev => ({ ...prev, mode: 'full', content: data?.nginx_config || '' }));
    }
  }

  async function saveWebsiteSettings() {
    if (!nginxCustomEditing) return;
    const original = nginxCustomEditing.site || {};
    const body = {};
    const nextAppType = websiteSettingsForm.app_type || original.app_type || 'wordpress';
    const nextPhp = websiteSettingsForm.php_version || original.php_version || '8.4';
    const nextRewrite = nextAppType === 'wordpress'
      ? 'front_controller'
      : nextAppType === 'static'
        ? 'none'
        : websiteSettingsForm.nginx_rewrite_mode || 'none';

    if (nextAppType !== (original.app_type || 'wordpress')) body.app_type = nextAppType;
    if (nextAppType !== 'static' && nextPhp !== original.php_version) body.php_version = nextPhp;
    if (nextRewrite !== (original.nginx_rewrite_mode || (original.app_type === 'wordpress' ? 'front_controller' : 'none'))) {
      body.nginx_rewrite_mode = nextRewrite;
    }
    if (Object.keys(body).length === 0) return;

    const data = await request(`/websites/${nginxCustomEditing.id}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }, `Saving ${nginxCustomEditing.domain} settings...`);
    if (data) {
      setNotice(`Updated settings for ${nginxCustomEditing.domain}.`);
      setWebsiteSettingsForm(websiteConfigForm(data));
      setNginxCustomEditing(prev => prev ? ({ ...prev, site: data }) : prev);
      await refreshAll();
    }
  }

  async function loadWebsiteLog(siteOrId = logViewer?.id, kind = logViewer?.kind || 'access', lines = logViewer?.lines || 200, domainLabel = logViewer?.domain || '') {
    const websiteId = typeof siteOrId === 'object' ? siteOrId.id : siteOrId;
    const domainName = typeof siteOrId === 'object' ? siteOrId.domain : domainLabel;
    if (!websiteId) return;
    const data = await request(`/websites/${websiteId}/logs?kind=${encodeURIComponent(kind)}&lines=${encodeURIComponent(lines)}`, {}, `Loading ${kind} log...`);
    if (data) {
      setLogViewer({
        id: websiteId,
        domain: data.domain || domainName,
        kind: data.kind || kind,
        lines: data.lines || lines,
        path: data.path || '',
        content: data.content || '',
        exists: !!data.exists,
      });
    }
  }

  async function openWebsiteLogs(site) {
    setNginxCustomEditing(null);
    setTerminalViewer(null);
    setLogViewer({ id: site.id, domain: site.domain, kind: 'access', lines: 200, path: '', content: '', exists: true });
    await loadWebsiteLog(site, 'access', 200, site.domain);
  }

  function openWebsiteTerminal(site) {
    setNginxCustomEditing(null);
    setLogViewer(null);
    setTerminalViewer({ id: site.id, domain: site.domain });
  }

  async function toggleWebsiteWaf(site) {
    const next = !site.waf_enabled;
    const data = await request(`/websites/${site.id}/waf`, {
      method: 'PATCH',
      body: JSON.stringify({ waf_enabled: next }),
    }, `${next ? 'Enabling' : 'Disabling'} WAF for ${site.domain}...`);
    if (data) {
      setNotice(`${next ? 'Enabled' : 'Disabled'} WAF for ${site.domain}.`);
      await refreshAll();
      if (String(selectedWafWebsiteId) === String(site.id)) await loadWebsiteWafConfig(site.id, false);
    }
  }

  async function fixWordPressPermissions(id) {
    const data = await request(`/maintenance/wordpress/${id}/fix-permissions`, { method: 'POST' }, 'Fixing permissions...');
    if (data?.message) setNotice(data.message);
  }

  async function fixNginxSecurity(id) {
    const data = await request(`/websites/${id}/fix-nginx-security`, { method: 'POST' }, 'Rewriting webserver security template...');
    if (data?.message) setNotice(data.message);
  }

  async function changeDbPassword(id) {
    const newPass = prompt('Enter a new database password, minimum 12 characters:');
    if (!newPass) return;
    await request(`/databases/${id}/password`, { method: 'POST', body: JSON.stringify({ password: newPass }) }, 'Changing database password...');
  }

  async function deleteDatabase(id, dbName) {
    if (!confirm(`Delete database "${dbName}"? This action cannot be undone.`)) return;
    const data = await request(`/databases/${id}`, { method: 'DELETE' }, 'Deleting database...');
    if (data) {
      setNotice(`Database "${dbName}" deleted successfully.`);
      await refreshAll();
    }
  }

  function generateRandomPassword(length = 20) {
    const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#%^*_+-';
    const arr = new Uint8Array(length);
    crypto.getRandomValues(arr);
    return Array.from(arr, b => chars[b % chars.length]).join('');
  }

  async function createDatabase() {
    const validDbName = /^[a-zA-Z0-9_]+$/;
    const dbName = newDatabase.db_name.trim();
    const dbUser = newDatabase.db_user.trim();
    const dbPass = newDatabase.db_password.trim();
    if (!dbName) { setError('Please enter a database name.'); return; }
    if (!validDbName.test(dbName)) { setError('Database name can only contain letters, numbers and underscores (no spaces or special characters).'); return; }
    if (dbUser && !validDbName.test(dbUser)) { setError('Database user can only contain letters, numbers and underscores (no spaces or special characters).'); return; }
    if (dbPass && dbPass.length < 12) { setError('Password must be at least 12 characters.'); return; }
    if (dbPass && /[^\x20-\x7E]/.test(dbPass)) { setError('Password contains invalid characters. Use only ASCII characters.'); return; }
    const body = {
      db_name: dbName,
      db_user: dbUser || null,
      db_password: dbPass || null,
    };
    const data = await request('/databases', { method: 'POST', body: JSON.stringify(body) }, 'Creating database...');
    if (data) {
      setCreatedDbInfo({ db_name: data.db_name, db_user: data.db_user, db_password: data.db_password });
      setNewDatabase({ db_name: '', db_user: '', db_password: '' });
      await refreshAll();
    }
  }

  async function addCron() {
    const data = await request('/maintenance/cron', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId), schedule: cronSchedule, command: cronCommand }) }, 'Adding cron job...');
    if (data) {
      if (data.cron_user) setCronUser(data.cron_user);
      setNotice(`Cron job added${data.cron_user ? ` as ${data.cron_user}` : ''}.`);
      await listCron();
    }
  }

  async function listCron() {
    if (!selectedWebsiteId) return;
    const data = await request(`/maintenance/cron/${selectedWebsiteId}`, {}, 'Loading cron jobs...');
    if (data?.items) setCronItems(data.items);
    if (data?.cron_user) setCronUser(data.cron_user);
  }

  async function deleteCron(index) {
    if (!confirm(`Delete cron #${index}?`)) return;
    index = Number(index);
    if (Number.isNaN(index)) return;
    const data = await request('/maintenance/cron', { method: 'DELETE', body: JSON.stringify({ website_id: Number(selectedWebsiteId), index }) }, 'Deleting cron job...');
    if (data) {
      if (data.cron_user) setCronUser(data.cron_user);
      setNotice('Cron job deleted.');
      await listCron();
    }
  }

  async function listFiles(path = fileListPath, websiteId = selectedWebsiteId) {
    if (!websiteId) return;
    const data = await request(`/maintenance/files/${websiteId}?path=${encodeURIComponent(path)}`, {}, 'Loading file list...');
    if (data?.items) { setFiles(data.items); setFileListPath(path); setFileUploadDir(path || ''); setSelectedFilePaths([]); }
  }

  async function readFile(pathOverride = filePath, websiteId = selectedWebsiteId) {
    const targetPath = pathOverride || filePath;
    if (!websiteId || !targetPath) return;
    if (pathOverride) setFilePath(pathOverride);
    const data = await request(`/maintenance/files/${websiteId}/read?path=${encodeURIComponent(targetPath)}`, {}, 'Reading file...');
    if (data?.content !== undefined) {
      setFileContent(data.content);
      setEditorCursor({ line: 1, column: 1 });
    }
  }

  async function writeFile() {
    const data = await request('/maintenance/files/write', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId), path: filePath, content: fileContent }) }, 'Saving file...');
    if (data) { await listFiles(fileListPath); await loadCurrentUser(); }
  }

  async function downloadFile(path) {
    if (!selectedWebsiteId || !path) return;
    try {
      setError(''); setLoading('Downloading file...');
      const res = await fetch(`${API}/maintenance/files/${selectedWebsiteId}/download?path=${encodeURIComponent(path)}`, { credentials: 'include' });
      if (!res.ok) { const data = await res.json().catch(() => ({})); if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Download failed.')); return; }
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url; link.download = path.split('/').pop() || 'download';
      document.body.appendChild(link); link.click(); link.remove();
      URL.revokeObjectURL(url);
    } catch (err) { setError('File download failed.'); }
    finally { setLoading(''); }
  }

  function fileEditorUrl(websiteId, path) {
    const url = new URL(window.location.href);
    url.pathname = routeForPage('files');
    url.search = '';
    url.hash = '';
    url.searchParams.set('view', 'editor');
    url.searchParams.set('website_id', String(websiteId));
    url.searchParams.set('path', path);
    return url.toString();
  }

  function openFileEditorTab(path, websiteId = selectedWebsiteId) {
    if (!websiteId || !path) return;
    window.open(fileEditorUrl(websiteId, path), '_blank', 'noopener,noreferrer');
  }

  async function makeFileDirectory() {
    if (!selectedWebsiteId) return;
    const name = prompt('Folder name:');
    if (!name) return;
    const data = await request('/maintenance/files/mkdir', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId), path: fileListPath || '', name }) }, 'Creating folder...');
    if (data) await listFiles(fileListPath);
  }

  async function makeFile() {
    if (!selectedWebsiteId) return;
    const name = prompt('File name:', 'new-file.txt');
    if (!name) return;
    const data = await request('/maintenance/files/create', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId), path: fileListPath || '', name }) }, 'Creating file...');
    if (data) {
      await listFiles(fileListPath);
      const newPath = [fileListPath, name].filter(Boolean).join('/');
      openFileEditorTab(newPath);
    }
  }

  async function renameFileItem(item) {
    if (!item) return;
    const newName = prompt('New name:', item.name);
    if (!newName || newName === item.name) return;
    const data = await request('/maintenance/files/rename', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId), path: item.path, new_name: newName }) }, 'Renaming...');
    if (data) await listFiles(fileListPath);
  }

  async function deleteSelectedFiles() {
    if (selectedFilePaths.length === 0) return;
    if (!confirm(`Delete ${selectedFilePaths.length} selected item(s)?`)) return;
    const data = await request('/maintenance/files/delete', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId), paths: selectedFilePaths }) }, 'Deleting selected files...');
    if (data) { await listFiles(fileListPath); await loadCurrentUser(); }
  }

  async function transferFileItems(action, paths) {
    if (!selectedWebsiteId || !paths?.length) return;
    const verb = action === 'copy' ? 'Copy' : 'Move';
    const destination = prompt(`${verb} to folder:`, fileListPath || 'public_html');
    if (destination === null) return;
    const targetPath = destination.trim() || fileListPath || 'public_html';
    const data = await request(`/maintenance/files/${action}`, {
      method: 'POST',
      body: JSON.stringify({ website_id: Number(selectedWebsiteId), paths, destination_path: targetPath }),
    }, `${verb}ing files...`);
    if (data) { await listFiles(fileListPath); await loadCurrentUser(); }
  }

  async function copySelectedFiles() {
    await transferFileItems('copy', selectedFilePaths);
  }

  async function moveSelectedFiles() {
    await transferFileItems('move', selectedFilePaths);
  }

  async function archiveSelectedFiles() {
    if (selectedFilePaths.length === 0) return;
    const ext = archiveFormat === 'tar.gz' ? 'tar.gz' : 'zip';
    const outputName = prompt('Archive file name:', `archive-${Date.now()}.${ext}`);
    if (!outputName) return;
    const data = await request('/maintenance/files/archive', {
      method: 'POST',
      body: JSON.stringify({ website_id: Number(selectedWebsiteId), base_path: fileListPath || '', paths: selectedFilePaths, output_name: outputName, format: archiveFormat }),
    }, 'Creating archive...');
    if (data) { await listFiles(fileListPath); await loadCurrentUser(); }
  }

  function isExtractableArchive(item) {
    if (!item || item.is_dir) return false;
    const name = String(item.name || item.path || '').toLowerCase();
    return name.endsWith('.zip') || name.endsWith('.tar.gz') || name.endsWith('.tgz');
  }

  async function extractArchive(item) {
    if (!isExtractableArchive(item) || !selectedWebsiteId) return;
    const defaultDestination = parentFilePath(item.path);
    const destination = prompt('Extract to folder:', defaultDestination);
    if (destination === null) return;
    const destinationPath = destination.trim();
    const data = await request('/maintenance/files/extract', {
      method: 'POST',
      body: JSON.stringify({
        website_id: Number(selectedWebsiteId),
        archive_path: item.path,
        destination_path: destinationPath,
      }),
    }, 'Starting extraction...');
    if (data?.job_id) {
      upsertFileJob(data);
      setSelectedFilePaths([]);
    }
  }

  async function extractSelectedArchive() {
    if (selectedFilePaths.length !== 1) return;
    const item = files.find(file => file.path === selectedFilePaths[0]);
    await extractArchive(item);
  }

  function upsertFileJob(job) {
    if (!job?.job_id) return;
    setFileJobs(prev => [job, ...prev.filter(item => item.job_id !== job.job_id)].slice(0, 6));
  }

  async function loadFileJob(jobId) {
    try {
      const res = await fetch(`${API}/maintenance/files/jobs/${jobId}`, { credentials: 'include' });
      const text = await res.text();
      let data;
      try { data = text ? JSON.parse(text) : {}; } catch { data = { detail: text || `HTTP ${res.status}` }; }
      if (!res.ok && handleAuthExpired(res.status, data.detail)) return null;
      if (!res.ok) return null;
      return data;
    } catch {
      return null;
    }
  }

  async function loadFileJobs(websiteId = selectedWebsiteId) {
    if (!websiteId) return;
    const data = await request(`/maintenance/files/jobs?website_id=${encodeURIComponent(websiteId)}`);
    if (data?.jobs) {
      setFileJobs(prev => [
        ...data.jobs,
        ...prev.filter(job => String(job.website_id) !== String(websiteId)),
      ].slice(0, 6));
    }
  }

  useEffect(() => {
    const activeJobs = fileJobs.filter(job => ['queued', 'running'].includes(job.status));
    if (activeJobs.length === 0) return undefined;

    const poll = async () => {
      for (const job of activeJobs) {
        const data = await loadFileJob(job.job_id);
        if (!data) continue;
        upsertFileJob(data);
        if (data.status === 'done') {
          setNotice(data.message || 'Extraction completed');
          await listFiles(data.destination_path || fileListPath);
          await loadCurrentUser();
        } else if (data.status === 'error') {
          setError(formatApiError(data.error, 'Extraction failed'));
        }
      }
    };

    const timer = window.setInterval(poll, 3000);
    return () => window.clearInterval(timer);
  }, [fileJobs]);

  useEffect(() => {
    if (page === 'files' && selectedWebsiteId) loadFileJobs(selectedWebsiteId);
  }, [page, selectedWebsiteId]);

  async function openWebsiteFileManager(site) {
    setNginxCustomEditing(null);
    setLogViewer(null);
    setTerminalViewer(null);
    setSelectedWebsiteId(String(site.id));
    navigateToPage('files');
    setFileListPath('public_html');
    setFileUploadDir('public_html');
    await listFiles('public_html', site.id);
  }

  async function uploadSiteFile(file) {
    if (!file) return;
    if (!selectedWebsiteId) { setError('Please select a website first.'); return; }
    const uploadDir = fileUploadDir.trim();
    const form = new FormData();
    form.append('file', file);
    try {
      setError('');
      setLoading('Uploading file...');
      const csrfToken = readCookie('opanel_ent_csrf');
      const headers = csrfToken ? { 'X-CSRF-Token': csrfToken } : {};
      const res = await fetch(`${API}/maintenance/files/${selectedWebsiteId}/upload?path=${encodeURIComponent(uploadDir)}`, {
        method: 'POST',
        credentials: 'include',
        headers,
        body: form,
      });
      const responseText = await res.text();
      let data;
      try { data = responseText ? JSON.parse(responseText) : {}; } catch { data = { detail: responseText || `HTTP ${res.status}` }; }
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Upload failed.')); return; }
      setNotice(`Uploaded ${file.name} to ${uploadDir || 'site root'}.`);
      if (String(fileListPath || '') === uploadDir) await listFiles(uploadDir);
      await loadCurrentUser();
    } catch (err) { setError('File upload failed.'); }
    finally { setLoading(''); }
  }

  async function createBackup() {
    const data = await request('/maintenance/backup', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId) }) }, 'Queueing backup...');
    if (data?.job_id) { setNotice('Backup queued. It will keep running on the server.'); await loadBackupJobs(); }
    else if (data?.backup_file) { setNotice(`Created backup: ${data.backup_file}`); await listBackups(); }
  }

  async function listBackups() {
    const data = await request(`/maintenance/backups/${selectedWebsiteId}`);
    if (data?.items) setBackups(data.items);
  }

  async function loadBackupJobs(showLoading = false) {
    const data = await request('/maintenance/backup-jobs', {}, showLoading ? 'Loading backup logs...' : '');
    if (data?.jobs) {
      const hasActive = data.jobs.some(job => ['queued', 'running'].includes(job.status));
      setBackupJobs(prev => {
        const hadActive = prev.some(job => ['queued', 'running'].includes(job.status));
        if (hadActive && !hasActive) {
          setTimeout(() => {
            if (selectedWebsiteId) listBackups();
            if (selectedBackupUserId) listUserBackups(selectedBackupUserId);
          }, 0);
        }
        return data.jobs;
      });
    }
  }

  async function refreshBackupArea() {
    await listBackups();
    await loadBackupJobs();
    if (selectedBackupUserId) await listUserBackups(selectedBackupUserId);
  }

  async function refreshUserBackupArea() {
    await loadUsers();
    await loadRestoreBackups();
    await loadBackupJobs();
    if (selectedBackupUserId) await listUserBackups(selectedBackupUserId);
  }

  async function refreshScheduledBackupArea() {
    await loadUsers();
    await loadSftpTargets();
    await loadBackupSchedules();
    await loadBackupJobs();
  }

  async function listUserBackups(userId = selectedBackupUserId) {
    if (!userId) return;
    const data = await request(`/maintenance/user-backups/${userId}`);
    if (data?.items) setUserBackups(data.items);
  }

  async function createUserBackup() {
    if (!selectedBackupUserId) return;
    const body = {
      user_id: Number(selectedBackupUserId),
      target_id: selectedSftpTargetId ? Number(selectedSftpTargetId) : null,
    };
    const data = await request('/maintenance/user-backup', { method: 'POST', body: JSON.stringify(body) }, 'Queueing full user backup...');
    if (data?.job_id) { setNotice('Full user backup queued. It will keep running on the server.'); await loadBackupJobs(); }
    else if (data?.backup_file) {
      setNotice(data.remote_file ? `Full user backup uploaded: ${data.remote_file}` : `Created full user backup: ${data.backup_file}`);
      await listUserBackups();
    }
  }

  async function loadBackupSchedules() {
    const data = await request('/maintenance/backup-schedules');
    if (data) setBackupSchedules(data);
  }

  async function loadRestoreBackups() {
    const data = await request('/maintenance/user-restore-backups');
    if (data?.items) setRestoreBackups(data.items);
    if (data?.directory) setRestoreBackupDir(data.directory);
  }

  async function createBackupSchedule() {
    const selectedUserIds = (newBackupSchedule.user_ids || []).map(Number).filter(Boolean);
    if (!newBackupSchedule.all_users && selectedUserIds.length === 0) return;
    const body = {
      user_id: selectedUserIds[0] || null,
      user_ids: newBackupSchedule.all_users ? [] : selectedUserIds,
      all_users: !!newBackupSchedule.all_users,
      schedule: newBackupSchedule.schedule,
      target_id: newBackupSchedule.target_id ? Number(newBackupSchedule.target_id) : null,
      retention: Number(newBackupSchedule.retention || 7),
      is_active: true,
    };
    const data = await request('/maintenance/backup-schedules', { method: 'POST', body: JSON.stringify(body) }, 'Saving backup schedule...');
    if (data) {
      setNotice('Backup schedule saved.');
      await loadBackupSchedules();
    }
  }

  async function deleteBackupSchedule(id) {
    if (!confirm('Delete this backup schedule?')) return;
    const data = await request(`/maintenance/backup-schedules/${id}`, { method: 'DELETE' }, 'Deleting backup schedule...');
    if (data) await loadBackupSchedules();
  }

  async function loadSftpTargets() {
    const data = await request('/maintenance/sftp-targets');
    if (data) {
      setSftpTargets(data);
      if (!selectedSftpTargetId && data[0]) setSelectedSftpTargetId(String(data[0].id));
    }
  }

  async function createSftpTarget() {
    const body = {
      ...newSftpTarget,
      port: Number(newSftpTarget.port || 22),
      password: newSftpTarget.password || null,
      private_key: newSftpTarget.private_key || null,
    };
    const data = await request('/maintenance/sftp-targets', { method: 'POST', body: JSON.stringify(body) }, 'Saving SFTP target...');
    if (data) {
      setNotice(`Saved SFTP target ${data.name}`);
      setNewSftpTarget({ name: '', host: '', port: 22, username: '', password: '', private_key: '', remote_path: '/backups/opanel-ent' });
      await loadSftpTargets();
    }
  }

  async function deleteSftpTarget(id) {
    if (!confirm('Delete this SFTP target?')) return;
    const data = await request(`/maintenance/sftp-targets/${id}`, { method: 'DELETE' }, 'Deleting SFTP target...');
    if (data) await loadSftpTargets();
  }

  async function createSftpBackup() {
    if (!selectedWebsiteId || !selectedSftpTargetId) return;
    const data = await request('/maintenance/backup-sftp', {
      method: 'POST',
      body: JSON.stringify({ website_id: Number(selectedWebsiteId), target_id: Number(selectedSftpTargetId) }),
    }, 'Queueing SFTP backup...');
    if (data?.job_id) {
      setNotice('SFTP backup queued. It will keep running on the server.');
      await loadBackupJobs();
    } else if (data?.remote_file) {
      setNotice(`SFTP backup uploaded: ${data.remote_file}`);
      await listBackups();
    }
  }

  async function restoreBackup(file) {
    if (!confirm(`Restore this backup to the current website?\n${file}`)) return;
    await request('/maintenance/restore', { method: 'POST', body: JSON.stringify({ website_id: Number(selectedWebsiteId), backup_file: file }) }, 'Restoring backup...');
  }

  async function downloadBackup(file) {
    if (!selectedWebsiteId) return;
    try {
      setError(''); setLoading('Downloading backup...');
      const res = await fetch(`${API}/maintenance/backups/${selectedWebsiteId}/download?backup_file=${encodeURIComponent(file)}`, { credentials: 'include' });
      if (!res.ok) { const data = await res.json().catch(() => ({})); if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Download failed.')); return; }
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url; link.download = file.split('/').pop() || 'backup.tar.gz';
      document.body.appendChild(link); link.click(); link.remove();
      URL.revokeObjectURL(url);
      setNotice('Backup downloaded.');
    } catch (err) { setError('Backup download failed.'); }
    finally { setLoading(''); }
  }

  async function downloadUserBackup(file) {
    try {
      setError(''); setLoading('Downloading full user backup...');
      const res = await fetch(`${API}/maintenance/user-backups-download?backup_file=${encodeURIComponent(file)}`, { credentials: 'include' });
      if (!res.ok) { const data = await res.json().catch(() => ({})); if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Download failed.')); return; }
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url; link.download = file.split('/').pop() || 'user-backup.tar.gz';
      document.body.appendChild(link); link.click(); link.remove();
      URL.revokeObjectURL(url);
      setNotice('Full user backup downloaded.');
    } catch (err) { setError('Full user backup download failed.'); }
    finally { setLoading(''); }
  }

  async function restoreUserBackup(file) {
    if (!confirm(`Restore this full user backup? Missing panel user and websites will be created.\n${file}`)) return;
    const data = await request('/maintenance/user-restore', { method: 'POST', body: JSON.stringify({ backup_file: file }) }, 'Restoring full user backup...');
    if (data) {
      setNotice(`Restored user ${data.username}. Websites: ${data.websites?.length || 0}`);
      await refreshAll();
      await loadUsers();
      await listUserBackups();
      await loadRestoreBackups();
    }
  }

  async function deleteUserBackup(file) {
    if (!confirm(`Delete this full user backup?\n${file}`)) return;
    const data = await request(`/maintenance/user-backups?backup_file=${encodeURIComponent(file)}`, { method: 'DELETE' }, 'Deleting full user backup...');
    if (data) {
      await listUserBackups();
      await loadRestoreBackups();
    }
  }

  async function deleteRestoreBackup(file) {
    if (!confirm(`Delete this restore backup?\n${file}`)) return;
    const data = await request(`/maintenance/user-restore-backups?backup_file=${encodeURIComponent(file)}`, { method: 'DELETE' }, 'Deleting restore backup...');
    if (data) {
      await loadRestoreBackups();
      await listUserBackups();
    }
  }

  async function uploadUserBackups(files) {
    const selectedFiles = Array.from(files || []);
    if (selectedFiles.length === 0) return;
    const form = new FormData();
    selectedFiles.forEach(file => form.append('files', file));
    try {
      setError(''); setLoading('Uploading full user backups...');
      const csrfToken = readCookie('opanel_ent_csrf');
      const headers = csrfToken ? { 'X-CSRF-Token': csrfToken } : {};
      const res = await fetch(`${API}/maintenance/user-restore-backups/upload`, {
        method: 'POST',
        credentials: 'include',
        headers,
        body: form,
      });
      const responseText = await res.text();
      let data;
      try { data = responseText ? JSON.parse(responseText) : {}; } catch { data = { detail: responseText || `HTTP ${res.status}` }; }
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Upload failed.')); return; }
      setNotice(`Uploaded ${data.items?.length || selectedFiles.length} full user backup file(s).`);
      await loadRestoreBackups();
      await listUserBackups();
    } catch (err) { setError('Full user backup upload failed.'); }
    finally { setLoading(''); }
  }

  async function loadDaBackups() {
    try {
      setError('');
      const res = await fetch(`${API}/maintenance/da-backups`, { credentials: 'include' });
      const data = await res.json();
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Failed to load DA backups.')); return; }
      setDaBackups(data.items || []);
      setDaBackupDir(data.directory || '');
    } catch (err) { setError('Failed to load DA backups.'); }
  }

  async function loadDaImportJobs() {
    try {
      setError('');
      const res = await fetch(`${API}/maintenance/da-import/jobs`, { credentials: 'include' });
      const data = await res.json();
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; return; }
      setDaImportJobs(data.jobs || []);
    } catch {}
  }

  async function uploadDaBackups(selectedFiles) {
    if (!selectedFiles || !selectedFiles.length) return;
    try {
      setError(''); setLoading('Uploading DA backups...');
      const csrfToken = readCookie('opanel_ent_csrf');
      const headers = csrfToken ? { 'X-CSRF-Token': csrfToken } : {};
      const form = new FormData();
      Array.from(selectedFiles).forEach(file => form.append('files', file));
      const res = await fetch(`${API}/maintenance/da-backups/upload`, {
        method: 'POST', credentials: 'include', headers, body: form,
      });
      const data = await res.json();
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Upload failed.')); return; }
      setNotice(`Uploaded ${data.items?.length || selectedFiles.length} DA backup(s).`);
      await loadDaBackups();
    } catch (err) { setError('DA backup upload failed.'); }
    finally { setLoading(''); }
  }

  async function deleteDaBackup(backupFile) {
    if (!confirm('Delete this DA backup?')) return;
    try {
      setError('');
      const csrfToken = readCookie('opanel_ent_csrf');
      const headers = csrfToken ? { 'X-CSRF-Token': csrfToken } : {};
      const url = new URL(`${API}/maintenance/da-backups`, window.location.origin);
      url.searchParams.set('backup_file', backupFile);
      const res = await fetch(url, { method: 'DELETE', credentials: 'include', headers });
      const data = await res.json();
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Delete failed.')); return; }
      setNotice(`Deleted DA backup: ${data.deleted}`);
      await loadDaBackups();
    } catch (err) { setError('Failed to delete DA backup.'); }
  }

  async function startDaImport(backupFile) {
    if (!confirm(`Import DirectAdmin backup "${backupFile}"?\n\nThis will create panel users, websites, databases, and configure OLS vhosts.`)) return;
    try {
      setError(''); setLoading('Importing DA backup...');
      const csrfToken = readCookie('opanel_ent_csrf');
      const headers = csrfToken ? { 'X-CSRF-Token': csrfToken } : {};
      const url = new URL(`${API}/maintenance/da-import`, window.location.origin);
      url.searchParams.set('backup_file', backupFile);
      const res = await fetch(url, { method: 'POST', credentials: 'include', headers });
      const data = await res.json();
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Import failed.')); return; }
      setNotice(`DA import started: ${data.backup_file}`);
      await loadDaImportJobs();
      // Poll for completion
      const pollJob = async () => {
        let attempts = 0;
        const maxAttempts = 120; // 2 minutes max
        while (attempts < maxAttempts) {
          await new Promise(resolve => setTimeout(resolve, 1000));
          const pollRes = await fetch(`${API}/maintenance/da-import/jobs/${data.job_id}`, { credentials: 'include' });
          if (pollRes.ok) {
            const job = await pollRes.json();
            if (job.status === 'done') {
              setNotice(`DA import complete: ${job.message}`);
              await loadDaImportJobs();
              await loadDaBackups();
              return;
            } else if (job.status === 'error') {
              setError(`DA import failed: ${job.error || job.message}`);
              await loadDaImportJobs();
              return;
            }
          }
          attempts++;
        }
        await loadDaImportJobs();
      };
      pollJob();
    } catch (err) { setError('Failed to start DA import.'); }
    finally { setLoading(''); }
  }

  async function openPhpMyAdmin(databaseId) {
    try {
      setError(''); setLoading('Opening phpMyAdmin...');
      const csrfToken = readCookie('opanel_ent_csrf');
      const headers = csrfToken ? { 'X-CSRF-Token': csrfToken } : {};
      const res = await fetch(`${API}/databases/${databaseId}/phpmyadmin-sso`, {
        method: 'POST',
        credentials: 'include',
        headers,
      });
      const data = await res.json().catch(() => ({}));
      if (handleAuthExpired(res.status, data.detail)) return;
      if (!res.ok || !data.url) { setError(formatApiError(data.detail, 'Cannot open phpMyAdmin.')); return; }
      window.open(data.url, '_blank', 'noopener,noreferrer');
    } catch (err) { setError('Cannot open phpMyAdmin.'); }
    finally { setLoading(''); }
  }

  async function downloadDatabase(databaseId, databaseName) {
    try {
      setError(''); setLoading('Downloading database...');
      const res = await fetch(`${API}/databases/${databaseId}/download`, { credentials: 'include' });
      if (!res.ok) { const data = await res.json().catch(() => ({})); if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Download failed.')); return; }
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url; link.download = `${databaseName || 'database'}.sql`;
      document.body.appendChild(link); link.click(); link.remove();
      URL.revokeObjectURL(url);
      setNotice('Database SQL downloaded.');
    } catch (err) { setError('Database download failed.'); }
    finally { setLoading(''); }
  }

  async function deleteBackup(file) {
    if (!confirm(`Delete this backup?\n${file}`)) return;
    const data = await request(`/maintenance/backups/${selectedWebsiteId}?backup_file=${encodeURIComponent(file)}`, { method: 'DELETE' }, 'Deleting backup...');
    if (data) await listBackups();
  }

  async function uploadBackup(file) {
    if (!file || !selectedWebsiteId) return;
    const form = new FormData();
    form.append('file', file);
    try {
      setError(''); setLoading('Uploading backup...');
      const csrfToken = readCookie('opanel_ent_csrf');
      const headers = csrfToken ? { 'X-CSRF-Token': csrfToken } : {};
      const res = await fetch(`${API}/maintenance/backups/${selectedWebsiteId}/upload`, {
        method: 'POST',
        credentials: 'include',
        headers,
        body: form,
      });
      const responseText = await res.text();
      let data;
      try { data = responseText ? JSON.parse(responseText) : {}; } catch { data = { detail: responseText || `HTTP ${res.status}` }; }
      if (!res.ok) { if (handleAuthExpired(res.status, data.detail)) return; setError(formatApiError(data.detail, 'Upload failed.')); return; }
      if (data.backup_file) { setNotice(`Uploaded backup: ${data.backup_file}`); await listBackups(); }
    } catch (err) { setError('Upload backup failed.'); }
    finally { setLoading(''); }
  }

  async function checkService(name) {
    const data = await request('/services/action', { method: 'POST', body: JSON.stringify({ name, action: 'status' }) });
    setServiceStates(prev => ({ ...prev, [name]: data || { stdout: '', stderr: error || 'Cannot check', returncode: 1 } }));
    return data;
  }

  async function loadServiceNames() {
    const data = await request('/services/list');
    const names = data?.services?.length ? data.services : serviceNames;
    setServiceNames(names);
    return names;
  }

  async function checkAllServices() {
    setLoading('Checking services...');
    const names = await loadServiceNames();
    for (const name of names) { await checkService(name); }
    setLoading('');
  }

  async function runServiceAction(name, action) {
    await request('/services/action', { method: 'POST', body: JSON.stringify({ name, action }) }, `${action} ${name}...`);
    await checkService(name);
  }

  async function loadPhpConfig(version = phpConfig.php_version) {
    const data = await request(`/maintenance/php-config?php_version=${encodeURIComponent(version)}`, {}, 'Loading PHP config...');
    if (data) setPhpConfig(prev => ({ ...prev, ...data, php_version: version }));
  }

  async function updatePhpConfig() {
    const data = await request('/maintenance/php-config', {
      method: 'POST',
      body: JSON.stringify({ ...phpConfig, max_execution_time: Number(phpConfig.max_execution_time), max_input_time: Number(phpConfig.max_input_time), max_input_vars: Number(phpConfig.max_input_vars), opcache_enable: !!phpConfig.opcache_enable }),
    }, 'Updating PHP config...');
    if (data?.target) { setNotice(`Updated PHP config: ${data.target}`); await loadPhpConfig(phpConfig.php_version); }
  }

  async function restorePhpDefaults() {
    if (!confirm(`Restore default PHP ${phpConfig.php_version} values?`)) return;
    const data = await request('/maintenance/php-config/defaults', {
      method: 'POST',
      body: JSON.stringify({ php_version: phpConfig.php_version }),
    }, 'Restoring PHP defaults...');
    if (data?.values) {
      setPhpConfig(prev => ({ ...prev, ...data.values }));
      setNotice(`Restored PHP ${phpConfig.php_version} defaults.`);
    }
  }

  async function loadPhpVersions() {
    const data = await request('/maintenance/php-versions', {}, 'Loading PHP versions...');
    if (data) setPhpVersions({
      installed: sortPhpVersions(data.installed || []),
      supported: sortPhpVersions(data.supported || []),
    });
  }

  async function installPhpVersion(version) {
    if (!confirm(`Install PHP ${version}? This will install lsphp${version.replace('.','')} via apt.`)) return;
    const data = await request(`/maintenance/php-versions/${version}/install`, { method: 'POST' }, `Installing PHP ${version}...`);
    if (data) { setNotice(`PHP ${version} installed successfully.`); await loadPhpVersions(); await loadServiceNames(); }
  }

  async function loadPhpTuning() {
    const data = await request(`/maintenance/php/tuning?php_version=${encodeURIComponent(phpConfig.php_version)}`, {}, 'Loading PHP tuning...');
    if (data) setPhpTuning(data);
  }

  async function applyPhpTuning() {
    if (!confirm(`Auto-tune PHP ${phpConfig.php_version} for this server? This will overwrite the OPanel Enterprise ini file and restart OpenLiteSpeed.`)) return;
    const data = await request(`/maintenance/php/tuning?php_version=${encodeURIComponent(phpConfig.php_version)}`, { method: 'POST' }, 'Applying PHP tuning...');
    if (data) {
      setNotice(`PHP tuned: memory_limit=${data.memory_limit}, OPcache=${data.opcache_memory_consumption}MB, LSAPI workers=${data.lsapi_children}`);
      setPhpTuning(null);
      await loadPhpConfig(phpConfig.php_version);
    }
  }

  async function loadFirewall() {
    const data = await request('/firewall/status', {}, 'Loading firewall...');
    if (data) setFirewallStatus(data);
  }

  async function runFirewallAction(path, options = {}, label = 'Updating firewall...') {
    const data = await request(path, options, label);
    if (data) { setNotice((data.stdout || data.stderr || 'Firewall updated.').trim()); await loadFirewall(); }
  }

  async function enableFirewall() {
    if (!confirm('Enable iptables firewall now? Make sure SSH and web ports are allowed.')) return;
    await runFirewallAction('/firewall/enable', { method: 'POST' }, 'Enabling firewall...');
  }
  async function disableFirewall() {
    if (!confirm('Disable iptables firewall?')) return;
    await runFirewallAction('/firewall/disable', { method: 'POST' }, 'Disabling firewall...');
  }
  async function reloadFirewall() { await runFirewallAction('/firewall/reload', { method: 'POST' }, 'Reloading firewall...'); }
  async function openFirewallPort() { await runFirewallAction('/firewall/allow-port', { method: 'POST', body: JSON.stringify({ port: firewallPort, protocol: firewallProtocol }) }, 'Opening port...'); }
  async function allowFirewallIp() { await runFirewallAction('/firewall/allow-ip', { method: 'POST', body: JSON.stringify({ ip: firewallAllowIp, port: firewallAllowPort || null, protocol: firewallAllowProtocol }) }, 'Allowing IP...'); }
  async function blockFirewallIp() {
    if (!confirm(`Block ${firewallBlockIp || 'this IP'}?`)) return;
    await runFirewallAction('/firewall/block-ip', { method: 'POST', body: JSON.stringify({ ip: firewallBlockIp, port: firewallBlockPort || null, protocol: firewallBlockProtocol }) }, 'Blocking IP...');
  }
  async function deleteFirewallRule(numberOverride = firewallDeleteNumber) {
    const ruleNumber = String(numberOverride || '').trim();
    if (!ruleNumber) return;
    if (!confirm(`Delete firewall rule #${ruleNumber}?`)) return;
    await runFirewallAction(`/firewall/rules/${encodeURIComponent(ruleNumber)}`, { method: 'DELETE' }, 'Deleting rule...');
    setFirewallDeleteNumber('');
  }

  function parseFirewallBlocklistUrls(text) {
    const lines = String(text || '').split('\n');
    const urls = [];
    let inUrls = false;
    for (const raw of lines) {
      const line = raw.trim();
      if (line === 'URLs:') { inUrls = true; continue; }
      if (line === 'Networks:' || line === 'Timer:') break;
      if (inUrls && /^https?:\/\//i.test(line)) urls.push(line);
    }
    return urls;
  }

  async function loadFirewallBlocklists() {
    const data = await request('/firewall/blocklists', {}, 'Loading blocklists...');
    if (data) setFirewallBlocklists(data);
  }

  async function addFirewallBlocklistUrl() {
    const url = firewallBlocklistUrl.trim();
    if (!url) return;
    const data = await request('/firewall/blocklists', { method: 'POST', body: JSON.stringify({ url }) }, 'Adding blocklist URL...');
    if (data) {
      setNotice((data.stdout || data.stderr || 'Blocklist URL added.').trim());
      setFirewallBlocklistUrl('');
      await loadFirewallBlocklists();
    }
  }

  async function deleteFirewallBlocklistUrl(url) {
    if (!confirm(`Delete blocklist URL?\n${url}`)) return;
    const data = await request('/firewall/blocklists/delete', { method: 'POST', body: JSON.stringify({ url }) }, 'Deleting blocklist URL...');
    if (data) {
      setNotice((data.stdout || data.stderr || 'Blocklist URL removed.').trim());
      await loadFirewallBlocklists();
    }
  }

  async function updateFirewallBlocklistsNow() {
    const data = await request('/firewall/blocklists/update', { method: 'POST' }, 'Refreshing blocklists...');
    if (data) {
      setNotice((data.stdout || data.stderr || 'Blocklists refreshed.').trim());
      await loadFirewall();
      await loadFirewallBlocklists();
    }
  }

  async function loadWafRules() {
    const data = await request('/waf/rules', {}, 'Loading WAF rules...');
    if (data) setWafRules(data);
  }

  function closeWafSiteConfig() {
    setWafSiteConfig(null);
    setSelectedWafWebsiteId('');
    setWafCustomRules('');
    setWafBotExtra('');
  }

  async function loadWebsiteWafConfig(websiteId = selectedWafWebsiteId, showLoading = true) {
    if (!websiteId) {
      setWafSiteConfig(null);
      return;
    }
    const data = await request(`/waf/websites/${websiteId}`, {}, showLoading ? 'Loading website WAF...' : '');
    if (data) {
      setSelectedWafWebsiteId(String(websiteId));
      setWafSiteConfig(data);
      setWafCustomRules(data.custom_rules || '');
      setWafBotExtra((data.bot_extra || []).join('\n'));
    }
  }

  function toggleWafDefaultRule(ruleId, enabled) {
    setWafSiteConfig(prev => {
      if (!prev) return prev;
      const current = new Set(prev.enabled_rule_ids || []);
      if (enabled) current.add(ruleId); else current.delete(ruleId);
      return {
        ...prev,
        enabled_rule_ids: Array.from(current),
        default_rules: (prev.default_rules || []).map(rule => rule.id === ruleId ? { ...rule, enabled } : rule),
      };
    });
  }

  async function saveWebsiteWafRules() {
    if (!selectedWafWebsiteId || !wafSiteConfig) return;
    const data = await request(`/waf/websites/${selectedWafWebsiteId}`, {
      method: 'PUT',
      body: JSON.stringify({
        enabled_rule_ids: wafSiteConfig.enabled_rule_ids || [],
        custom_rules: wafCustomRules,
        bot_blocking_enabled: !!wafSiteConfig.bot_blocking_enabled,
        bot_extra: wafBotExtra,
      }),
    }, 'Saving website WAF rules...');
    if (data) {
      setWafSiteConfig(data);
      setWafCustomRules(data.custom_rules || '');
      setWafBotExtra((data.bot_extra || []).join('\n'));
      setNotice(data.message || 'Website WAF rules saved.');
      await refreshAll();
    }
  }

  async function loadBadBots() {
    const data = await request('/waf/bad-bots', { silent: true }, '');
    if (data) {
      setBadBots({ ...data, patterns: data.patterns || [] });
      setBadBotText((data.patterns || []).join('\n'));
    }
  }

  async function saveBadBots() {
    const count = badBotText.split(/[\r\n,]+/).map(s => s.trim()).filter(s => s && !s.startsWith('#')).length;
    if (!confirm(
      `Apply this bad bot list to every website?\n\n${count} pattern(s).\n\n`
      + 'Requests whose User-Agent contains one of them get 403 on every site that has bot blocking on.'
    )) return;
    const data = await request('/waf/bad-bots', {
      method: 'PUT',
      body: JSON.stringify({ patterns: badBotText }),
    }, 'Applying bad bot list to all websites...');
    if (data) {
      setBadBots({ ...data, patterns: data.patterns || [] });
      setBadBotText((data.patterns || []).join('\n'));
      setNotice(data.message || 'Bad bot list saved.');
      if (selectedWafWebsiteId) await loadWebsiteWafConfig(selectedWafWebsiteId, false);
    }
  }

  async function loadWafAccessLogs(nextFilters = wafAccessFilters, showLoading = true) {
    const filters = { ...wafAccessFilters, ...nextFilters };
    const params = new URLSearchParams();
    if (filters.domain) params.set('domain', filters.domain);
    if (filters.verdict) params.set('verdict', filters.verdict);
    if (filters.q) params.set('q', filters.q);
    params.set('limit', String(filters.limit || 50));
    params.set('offset', String(filters.offset || 0));
    const data = await request(`/waf/access-logs?${params.toString()}`, {}, showLoading ? 'Loading access logs...' : '');
    if (data) {
      setWafAccessFilters(filters);
      setWafAccessLogs(data);
    }
  }

  function exportWafAccessLogs() {
    const rows = wafAccessLogs.entries || [];
    const header = ['verdict', 'time', 'site', 'method', 'path', 'ip', 'reason', 'status', 'user_agent'];
    const csv = [
      header.join(','),
      ...rows.map(row => header.map(key => csvEscape(row[key])).join(',')),
    ].join('\n');
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = `opanel-ent-access-logs-${new Date().toISOString().slice(0, 10)}.csv`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  async function clearWafAccessLogs() {
    const domainLabel = wafAccessFilters.domain || 'all domains';
    if (!confirm(`Clear WAF access logs for ${domainLabel}?`)) return;
    const params = new URLSearchParams();
    if (wafAccessFilters.domain) params.set('domain', wafAccessFilters.domain);
    const suffix = params.toString() ? `?${params.toString()}` : '';
    const data = await request(`/waf/access-logs${suffix}`, { method: 'DELETE' }, 'Clearing access logs...');
    if (data) await loadWafAccessLogs({ ...wafAccessFilters, offset: 0 }, false);
  }

  async function loadUpdates(force = false) {
    const data = await request(`/updates/status${force ? '?refresh=true' : ''}`, {}, 'Loading update status...');
    if (data) setUpdatesStatus(data);
  }

  async function toggleUpdateLog() {
    if (!showUpdateLog && !updatesStatus) await loadUpdates();
    setShowUpdateLog(prev => !prev);
  }

  async function runOsUpdate() {
    if (!confirm('Run apt-get update && apt-get upgrade now?')) return;
    setOsUpdating(true);
    const data = await request('/updates/os/run', { method: 'POST' }, 'Updating OS packages...');
    setOsUpdating(false);
    if (data) { setNotice((data.stdout || data.stderr || 'OS update completed.').trim()); if (showUpdateLog) await loadUpdates(); }
  }

  async function saveOsAutoUpdate() {
    const data = await request('/updates/os/auto', { method: 'POST', body: JSON.stringify(osAutoUpdate) }, 'Saving OS auto update...');
    if (data) { setNotice((data.stdout || data.stderr || 'OS auto update saved.').trim()); if (showUpdateLog) await loadUpdates(); }
  }

  async function runPanelUpdate() {
    if (!confirm('Update opanel-ent from GitHub main now? The API may restart and this page will reload when done.')) return;
    setPanelUpdating(true);
    setShowUpdateLog(true);
    setPanelUpdateLog([]);
    const data = await request('/updates/panel/run', { method: 'POST' }, 'Updating opanel-ent...');
    if (!data) {
      setPanelUpdating(false);
      return;
    }
    // Poll /updates/status every 2s until the update finishes, then reload.
    // The API restarts (twice) mid-update, so /updates/status will fail for a
    // stretch -- tolerate that, but never spin forever: give up after a run of
    // failed polls or once an overall deadline passes, and clear the spinner so
    // the page does not get stuck showing "Running ...".
    const startedAt = Date.now();
    const DEADLINE_MS = 30 * 60 * 1000;
    const MAX_CONSECUTIVE_FAILURES = 45; // ~90s of the API being unreachable
    let consecutiveFailures = 0;
    let finished = false;
    const stopPolling = () => {
      finished = true;
      if (panelUpdateInterval.current) {
        clearInterval(panelUpdateInterval.current);
        panelUpdateInterval.current = null;
      }
      setPanelUpdating(false);
    };
    const pollOnce = async () => {
      const status = await request('/updates/status', {}, null);
      if (!status) {
        consecutiveFailures += 1;
        if (consecutiveFailures >= MAX_CONSECUTIVE_FAILURES || Date.now() - startedAt > DEADLINE_MS) {
          stopPolling();
          setNotice('Lost contact with the panel while it was updating. It has most likely finished — reload the page (Ctrl+Shift+R) to see the result.');
        }
        return;
      }
      consecutiveFailures = 0;
      setUpdatesStatus(status);
      if (Array.isArray(status.panel_update_log)) {
        setPanelUpdateLog(status.panel_update_log);
      } else if (typeof status.panel_update_log === 'string' && status.panel_update_log) {
        setPanelUpdateLog(status.panel_update_log.split('\n'));
      }
      const st = status.panel || {};
      const done = st.last_update_status === 'completed' || st.last_update_status === 'failed';
      if (done) {
        stopPolling();
        if (st.last_update_status === 'completed' && Number(st.progress_percent) === 100) {
          setNotice('Panel update completed. Reloading to apply the new version...');
          setTimeout(() => { window.location.reload(); }, 2000);
        } else if (st.last_update_status === 'failed') {
          setNotice((st.progress_message || st.last_update_message || 'Panel update failed.').trim());
        }
        return;
      }
      if (Date.now() - startedAt > DEADLINE_MS) {
        stopPolling();
        setNotice('The panel update is taking longer than expected. Reload the page (Ctrl+Shift+R) to check whether it finished.');
      }
    };
    await pollOnce();
    if (panelUpdateInterval.current) clearInterval(panelUpdateInterval.current);
    if (!finished) panelUpdateInterval.current = setInterval(pollOnce, 2000);
  }

  async function savePanelAutoUpdate() {
    const data = await request('/updates/panel/auto', { method: 'POST', body: JSON.stringify(panelAutoUpdate) }, 'Saving panel auto update...');
    if (data) { setNotice((data.stdout || data.stderr || 'Panel auto update saved.').trim()); if (showUpdateLog) await loadUpdates(); }
  }

  useEffect(() => {
    if (isAuthenticated) {
      refreshAll();
    }
  }, [isAuthenticated]);

  useEffect(() => {
    if (standaloneEditor) return undefined;
    const syncPageFromLocation = () => setPage(pageFromPathname(window.location.pathname));
    syncPageFromLocation();
    window.addEventListener('popstate', syncPageFromLocation);
    return () => window.removeEventListener('popstate', syncPageFromLocation);
  }, [standaloneEditor]);

  useEffect(() => {
    if (!isAuthenticated || !standaloneEditor) return;
    setSelectedWebsiteId(standaloneEditor.websiteId);
    setFilePath(standaloneEditor.path);
    readFile(standaloneEditor.path, standaloneEditor.websiteId);
  }, [isAuthenticated, standaloneEditor]);

  useEffect(() => {
    if (!standaloneEditor || !isAuthenticated) return undefined;
    const handler = event => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') {
        event.preventDefault();
        writeFile();
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [standaloneEditor, isAuthenticated, selectedWebsiteId, filePath, fileContent]);

  useEffect(() => {
    if (!isAuthenticated || page !== 'dashboard' || !isAdmin) return undefined;
    loadResourceUsage();
    const timer = setInterval(loadResourceUsage, 5000);
    return () => clearInterval(timer);
  }, [isAuthenticated, page, isAdmin]);

  useEffect(() => {
    if (!isAuthenticated || page !== 'services') return undefined;
    checkAllServices();
    const timer = setInterval(checkAllServices, 10000);
    return () => clearInterval(timer);
  }, [isAuthenticated, page]);

  useEffect(() => {
    if (!isAuthenticated || page !== 'wafLogs') {
      wafAccessFiltersRef.current = wafAccessFilters;
      return undefined;
    }
    const prev = wafAccessFiltersRef.current;
    const qChanged = prev.q !== wafAccessFilters.q;
    const otherChanged =
      prev.domain !== wafAccessFilters.domain ||
      prev.verdict !== wafAccessFilters.verdict ||
      prev.limit !== wafAccessFilters.limit;
    const timer = window.setTimeout(() => {
      loadWafAccessLogs({ ...wafAccessFilters, offset: 0 }, false);
    }, qChanged && !otherChanged ? 200 : 0);
    wafAccessFiltersRef.current = wafAccessFilters;
    return () => window.clearTimeout(timer);
  }, [
    isAuthenticated,
    isAdmin,
    page,
    wafAccessFilters.domain,
    wafAccessFilters.verdict,
    wafAccessFilters.q,
    wafAccessFilters.limit,
  ]);

  useEffect(() => {
    if (!currentSite) return;
    const m = currentSite.ssl_mode;
    setSslMode(m === 'manual' ? 'manual' : m === 'reuse' ? 'existing' : currentSite.ssl_wildcard ? 'wildcard' : 'letsencrypt');
    setManualSslForm({ certificate: '', private_key: '', ca_bundle: '' });
    setManualSslFiles({ certificate: null, private_key: null, ca_bundle: null });
    setWildcardSslForm({ api_token: '', email: '' });
    setAvailableCerts([]);
    setReuseCertName(m === 'reuse' ? (currentSite.ssl_reuse_name || '') : '');
  }, [currentSite?.id]);

  useEffect(() => { if (selectedWebsiteId && page === 'backups') { listBackups(); loadBackupJobs(); } }, [selectedWebsiteId, page]);

  useEffect(() => { if (selectedWebsiteId && page === 'cron') listCron(); }, [selectedWebsiteId, page]);

  useEffect(() => { if (selectedWebsiteId && page === 'files') listFiles('public_html'); }, [selectedWebsiteId, page]);

  useEffect(() => { if (selectedBackupUserId && page === 'backups') listUserBackups(selectedBackupUserId); }, [selectedBackupUserId, page]);

  useEffect(() => {
    if (!isAuthenticated || page !== 'backups') return undefined;
    loadBackupJobs();
    const timer = setInterval(loadBackupJobs, 5000);
    return () => clearInterval(timer);
  }, [isAuthenticated, page, selectedWebsiteId, selectedBackupUserId]);

  useEffect(() => {
    if (isAuthenticated && page === 'users') { loadUsers(); loadPlans(); }
    if (isAuthenticated && page === 'php') { loadPhpConfig(); loadPhpVersions(); }
    if (isAuthenticated && page === 'firewall') { loadFirewall(); loadFirewallBlocklists(); }
    if (isAuthenticated && page === 'waf') { loadWafRules(); loadBadBots(); }
    if (isAuthenticated && page === 'updates' && currentUser?.role === 'admin') loadUpdates();
    if (isAuthenticated && page === 'security') {
      loadTwoFactorStatus();
    }
    if (isAuthenticated && page === 'malware' && isAdmin) {
      loadMalwareScanStatus();
      loadMalwareScanJobs();
      loadLatestMalwareScanJob();
      loadMalwareSchedule();
      loadQuarantine();
      if (!websites.length) refreshAll();
    }
    if (isAuthenticated && page === 'settings') { loadPanelSettings(); loadApiTokens(); loadNetworkStatus(); }
    if (isAuthenticated && page === 'backups' && currentUser?.role === 'admin') { loadUsers(); loadSftpTargets(); loadBackupSchedules(); loadRestoreBackups(); loadDaBackups(); loadDaImportJobs(); }
  }, [isAuthenticated, page, currentUser?.role]);

  useEffect(() => {
    if (!scanJob?.job_id || !['queued', 'running'].includes(scanJob.status)) return undefined;
    setScanLoading(true);
    const poll = () => loadMalwareScanJob(scanJob.job_id);
    const timer = window.setInterval(poll, 2000);
    poll();
    return () => window.clearInterval(timer);
  }, [scanJob?.job_id, scanJob?.status]);

  useEffect(() => {
    if (!isAuthenticated || page !== 'wafLogs' || !wafAccessAutoRefresh) return undefined;
    const timer = window.setInterval(() => {
      loadWafAccessLogs({ ...wafAccessFilters, offset: 0 }, false);
    }, wafAccessAutoRefresh * 1000);
    return () => window.clearInterval(timer);
  }, [
    isAuthenticated,
    isAdmin,
    page,
    wafAccessAutoRefresh,
    wafAccessFilters.domain,
    wafAccessFilters.verdict,
    wafAccessFilters.q,
    wafAccessFilters.limit,
  ]);

  useEffect(() => { setMobileMenuOpen(false); }, [page]);

  useEffect(() => {
    if (SETTINGS_PAGE_KEYS.includes(page)) setSettingsMenuOpen(true);
  }, [page]);

  function roleLabel(role) {
    return role === 'admin' ? 'Admin' : 'End user';
  }

  const mainNavItems = [
    ['dashboard', 'Dashboard', Home],
    ['websites', 'Websites', Globe],
    ['ssl', 'SSL', Lock],
    ['databases', 'Database', Database],
    ['cron', 'Cron', Clock],
    ['files', 'File manager', FolderOpen],
    ['backups', 'Backups', Archive],
    ...(isAdmin ? [['users', 'Panel users', Users]] : []),
  ];

  const settingsNavItems = [
    ...(isAdmin ? [['settings', 'Panel settings', SettingsIcon]] : []),
    ['security', 'Security', Shield],
    ...(isAdmin ? [['malware', 'Malware Scanner', Search]] : []),
    ...(isAdmin ? [['php', 'PHP config', Code2]] : []),
    ...(isAdmin ? [['firewall', 'Firewall', Shield]] : []),
    ['waf', 'WAF', Shield],
    ['wafLogs', 'Access Logs', FileText],
    ...(isAdmin ? [['updates', 'Updates', RefreshCw]] : []),
    ['services', 'Services Status', Server],
  ];

  const navItems = [...mainNavItems, ...settingsNavItems];
  const activeNavItem = navItems.find(([key]) => key === page) || navItems[0];
  const settingsIsActive = SETTINGS_PAGE_KEYS.includes(page);

  function renderNotifications() {
    const errorMessage = formatApiError(error, '').trim();
    const noticeMessage = formatApiError(notice, '').trim();
    if (!errorMessage && !noticeMessage) return null;
    return <div className="app-toast-stack" aria-label="Notifications">
      <NotificationToast type="error" message={errorMessage} onClose={() => setError('')} />
      <NotificationToast type="success" message={noticeMessage} onClose={() => setNotice('')} />
    </div>;
  }

  function websiteUrl(site) {
    const value = (site?.domain || '').trim();
    if (/^https?:\/\//i.test(value)) return value;
    return `${site?.ssl_enabled ? 'https' : 'http'}://${value}`;
  }

  function parentFilePath(path) {
    const parts = String(path || '').split('/').filter(Boolean);
    parts.pop();
    return parts.join('/');
  }

  function fileBreadcrumbs(path) {
    const parts = String(path || '').split('/').filter(Boolean);
    let current = '';
    return parts.map(part => {
      current = current ? `${current}/${part}` : part;
      return { label: part, path: current };
    });
  }

  function isTextEditable(item) {
    if (!item || item.is_dir) return false;
    const name = (item.name || '').toLowerCase();
    const editableDotfiles = new Set(['.env', '.env.example', '.htaccess', '.user.ini', '.gitignore', '.gitattributes']);
    return editableDotfiles.has(name) || /\.(txt|md|json|css|js|jsx|ts|tsx|html|htm|xml|yml|yaml|ini|conf|log|php|env|htaccess)$/.test(name) || !name.includes('.');
  }

  function toggleFileSelection(path) {
    setSelectedFilePaths(prev => prev.includes(path) ? prev.filter(item => item !== path) : [...prev, path]);
  }

  function toggleAllFiles() {
    setSelectedFilePaths(prev => prev.length === files.length ? [] : files.map(item => item.path));
  }

  function editorLanguage(path) {
    const name = String(path || '').toLowerCase();
    if (/\.php\d?$/.test(name) || name.endsWith('.phtml')) return 'PHP';
    if (/\.(js|jsx|ts|tsx)$/.test(name)) return 'JavaScript';
    if (/\.css$/.test(name)) return 'CSS';
    if (/\.html?$/.test(name)) return 'HTML';
    if (/\.json$/.test(name)) return 'JSON';
    if (/\.ya?ml$/.test(name)) return 'YAML';
    if (/\.(conf|ini|env|htaccess)$/.test(name)) return 'Config';
    return 'Text';
  }

  function WebsiteSelect() {
    return <select value={selectedWebsiteId} onChange={e => setSelectedWebsiteId(e.target.value)}>
      <option value="">-- Select website --</option>
      {websites.map(site => <option key={site.id} value={site.id}>{site.domain}</option>)}
    </select>;
  }

  function EmptyState({ icon: Icon = AlertCircle, message = 'No data yet', action }) {
    return <div className="empty-state">
      <Icon size={40} />
      <p>{message}</p>
      {action && <button type="button" className="mini secondary-light" onClick={action.onClick}>{action.icon ? <action.icon size={14}/> : null} {action.label}</button>}
    </div>;
  }

  function formatBytes(value) {
    const amount = Number(value);
    if (!Number.isFinite(amount) || amount < 0) return '--';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let size = amount;
    let unit = 0;
    while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit += 1; }
    return `${size >= 10 || unit === 0 ? size.toFixed(0) : size.toFixed(1)} ${units[unit]}`;
  }

  function formatPercent(value) {
    const amount = Number(value);
    if (!Number.isFinite(amount)) return '--';
    return `${Math.round(amount)}%`;
  }

  function clampPercent(value) {
    const amount = Number(value);
    if (!Number.isFinite(amount)) return 0;
    return Math.max(0, Math.min(100, amount));
  }

  function storageLimitBytes(user) {
    if (!user) return null;
    if (user.storage_limit_bytes === null) return null;
    if (user.storage_limit_bytes !== undefined) return user.storage_limit_bytes;
    return Number(user.storage_limit_mb || 0) * 1024 * 1024;
  }

  function storageUsageText(user) {
    const used = Number(user?.storage_used_bytes || 0);
    const limit = storageLimitBytes(user);
    if (limit === null) return `${formatBytes(used)}`;
    const pct = limit > 0 ? Math.round((used / limit) * 100) : 0;
    return `${formatBytes(used)}/${formatBytes(limit)} ${pct}%`;
  }

  function ResourceCard({ icon: Icon, label, value, detail, percent }) {
    const safePercent = percent == null ? null : clampPercent(percent);
    return <article className="resource-card">
      <div className="resource-head"><span className="resource-icon"><Icon size={16}/></span><span>{label}</span></div>
      <strong>{value}</strong>
      {safePercent !== null && <div className="resource-track"><span style={{ width: `${safePercent}%` }}></span></div>}
      <small>{detail}</small>
    </article>;
  }

  function renderDashboard() {
    const cpu = resourceUsage?.cpu || {};
    const memory = resourceUsage?.memory || {};
    const disk = resourceUsage?.disk || {};
    const network = resourceUsage?.network || {};
    const networkTotal = (Number(network.rx_per_sec) || 0) + (Number(network.tx_per_sec) || 0);
    return <>
      {isAdmin && <section className="resource-grid">
        <ResourceCard icon={Cpu} label="CPU" value={formatPercent(cpu.percent)} percent={cpu.percent} detail={cpu.load?.length ? `Load ${cpu.load.join(' / ')}` : `${cpu.cores || '--'} cores`} />
        <ResourceCard icon={MemoryStick} label="RAM" value={formatPercent(memory.percent)} percent={memory.percent} detail={`${formatBytes(memory.used)} / ${formatBytes(memory.total)}`} />
        <ResourceCard icon={HardDrive} label="Disk" value={formatPercent(disk.percent)} percent={disk.percent} detail={`${formatBytes(disk.used)} / ${formatBytes(disk.total)}`} />
        <ResourceCard icon={Network} label="Network" value={`${formatBytes(networkTotal)}/s`} detail={`Down ${formatBytes(network.rx_per_sec)}/s / Up ${formatBytes(network.tx_per_sec)}/s`} />
      </section>}
      <section className="stats-grid">
        <div className="stat-card"><strong>{websites.length}</strong><span>Websites</span></div>
        <div className="stat-card"><strong>{databases.length}</strong><span>Databases</span></div>
        <div className="stat-card"><strong>{websites.filter(s => s.ssl_enabled).length}</strong><span>SSL active</span></div>
        {currentUser && !isAdmin && <div className="stat-card"><strong>{formatBytes(currentUser.storage_used_bytes)}</strong><span>Storage / {formatBytes(storageLimitBytes(currentUser))}</span></div>}
      </section>
      {websites.length > 0 && <section className="section">
        <h2>Quick overview</h2>
        <div className="site-grid">
          {websites.slice(0, 4).map(site => <article className="site-card" key={site.id}>
            <div className="site-head">
              <div><a className="site-link" href={websiteUrl(site)} target="_blank" rel="noopener noreferrer">{site.domain}</a></div>
            </div>
            <div className="site-meta">
              <span className={`badge site-ssl-badge ${site.ssl_enabled ? 'ok' : ''}`}>{site.ssl_enabled ? 'SSL' : 'No SSL'}</span>
              <span>PHP <strong>{site.php_version}</strong></span>
              <span>Root <strong>{site.document_root || 'public_html'}</strong></span>
            </div>
          </article>)}
        </div>
        {websites.length > 4 && <p className="hint" style={{marginTop:8}}>Showing 4 of {websites.length} websites. Go to Websites for full list.</p>}
      </section>}
      {websites.length === 0 && <section className="section">
        <EmptyState icon={Globe} message="No websites yet." action={{ label: 'Go to Websites', icon: Globe, onClick: () => navigateToPage('websites') }} />
      </section>}
    </>;
  }

  function renderNginxEditor() {
    if (!nginxCustomEditing) return null;
    const fullConfig = nginxCustomEditing.mode === 'full';
    const selectedAppType = websiteSettingsForm.app_type || nginxCustomEditing.site?.app_type || 'wordpress';
    const rewriteDisabled = selectedAppType !== 'php';
    const settingsSite = nginxCustomEditing.site || {};
    const siteDomains = settingsSite.aliases || [];
    const aliasMode = aliasModes[nginxCustomEditing.id] || 'alias';
    return <section className="section nginx-modal inline-nginx-editor">
      <div className="section-title">
        <div className="nginx-config-title">
          <h2>{fullConfig ? 'VHost Config' : 'Website settings'} - {nginxCustomEditing.domain}</h2>
          <p className="hint">{fullConfig
            ? 'This is read-only. opanel-ent manages the main vhost template.'
            : 'Managed settings rewrite the main vhost safely.'}</p>
        </div>
        <div className="actions">
          {!fullConfig && isAdmin && <button className="secondary-light" disabled={!!loading} onClick={viewFullNginxConfig}><FileText size={14}/> View all</button>}
          {fullConfig && <button className="secondary-light" disabled={!!loading} onClick={() => setNginxCustomEditing(prev => ({ ...prev, mode: 'settings', content: '' }))}><SettingsIcon size={14}/> Settings</button>}
          <button className="secondary-light" onClick={() => setNginxCustomEditing(null)}><X size={14}/> Close</button>
        </div>
      </div>
      {!fullConfig && <div className="website-settings-grid">
        <label><span>Website mode</span><select
          value={websiteSettingsForm.app_type}
          onChange={e => setWebsiteSettingsForm(prev => ({
            ...prev,
            app_type: e.target.value,
            nginx_rewrite_mode: e.target.value === 'php' ? prev.nginx_rewrite_mode || 'none' : e.target.value === 'wordpress' ? 'front_controller' : 'none',
          }))}
          disabled={!!loading}
        >
          <option value="wordpress">WordPress</option>
          <option value="php">PHP</option>
          <option value="static">Static</option>
        </select></label>
        {selectedAppType !== 'static' && <label><span>PHP version</span><select
          value={websiteSettingsForm.php_version}
          onChange={e => setWebsiteSettingsForm(prev => ({ ...prev, php_version: e.target.value }))}
          disabled={!!loading}
        >
          {phpVersionOptions(phpVersions.installed, websiteSettingsForm.php_version).map(v => <option key={v} value={v}>PHP {v}</option>)}
        </select></label>}
        <label><span>Webserver rewrite</span><select
          value={rewriteDisabled ? (selectedAppType === 'wordpress' ? 'front_controller' : 'none') : websiteSettingsForm.nginx_rewrite_mode}
          onChange={e => setWebsiteSettingsForm(prev => ({ ...prev, nginx_rewrite_mode: e.target.value }))}
          disabled={!!loading || rewriteDisabled}
        >
          {NGINX_REWRITE_MODES.map(mode => <option key={mode.value} value={mode.value}>{mode.label}</option>)}
        </select></label>
        <div className="website-settings-actions">
          <button disabled={!!loading} onClick={saveWebsiteSettings}><Save size={14}/> Save settings</button>
        </div>
      </div>}
      {!fullConfig && <div className="site-aliases settings-domain-manager">
        <div className="domain-manager-head">
          <h3>Domains</h3>
          <p className="hint">Alias serves the same app. Redirect sends visitors to {nginxCustomEditing.domain}.</p>
        </div>
        <div className="alias-list">
          <span className="alias-chip primary-domain"><Globe size={12}/>{nginxCustomEditing.domain}<span>Main</span></span>
          {siteDomains.length === 0
            ? <span className="alias-empty">No extra domains</span>
            : siteDomains.map(alias => <span className="alias-chip" key={alias.id}>
              <Globe size={12}/>{alias.domain}<span>{alias.mode === 'redirect' ? 'Redirect' : 'Alias'}</span>
              <button type="button" disabled={!!loading} title={`Remove ${alias.domain}`} aria-label={`Remove ${alias.domain}`} onClick={() => deleteWebsiteAlias(settingsSite, alias)}><X size={12}/></button>
            </span>)}
        </div>
        <div className="alias-form settings-domain-form">
          <input
            value={aliasDrafts[nginxCustomEditing.id] || ''}
            onChange={e => setAliasDrafts(prev => ({ ...prev, [nginxCustomEditing.id]: e.target.value }))}
            onKeyDown={e => { if (e.key === 'Enter') addWebsiteAlias(settingsSite); }}
            placeholder="domain-alias.com"
            disabled={!!loading}
          />
          <select
            value={aliasMode}
            onChange={e => setAliasModes(prev => ({ ...prev, [nginxCustomEditing.id]: e.target.value }))}
            disabled={!!loading}
          >
            <option value="alias">Alias</option>
            <option value="redirect">Redirect</option>
          </select>
          <button className="secondary-light" disabled={!!loading || !(aliasDrafts[nginxCustomEditing.id] || '').trim()} onClick={() => addWebsiteAlias(settingsSite)}><Plus size={14}/> Add domain</button>
        </div>
      </div>}
      {fullConfig && <div className="custom-nginx-block">
        <textarea
          className="code-editor"
          value={nginxCustomEditing.content}
          placeholder={`docRoot                   /home/site/${nginxCustomEditing.domain}/public_html`}
          spellCheck={false}
          rows={18}
          readOnly
        />
      </div>}
      <div className="actions">
        <button className="secondary-light" disabled={!!loading} onClick={() => setNginxCustomEditing(null)}>{fullConfig ? 'Close' : 'Cancel'}</button>
      </div>
    </section>;
  }


  function renderWebsiteTerminal() {
    if (!terminalViewer) return null;
    return <section className="section nginx-modal terminal-modal">
      <div className="section-title">
        <h2>Terminal - {terminalViewer.domain}</h2>
        <button className="secondary-light" onClick={() => setTerminalViewer(null)}><X size={14}/> Close</button>
      </div>
      <div style={{ height: '500px', marginTop: '8px' }}>
        <Terminal websiteId={terminalViewer.id} apiBase={API} />
      </div>
    </section>;
  }

  function renderWebsiteLogViewer() {
    if (!logViewer) return null;
    return <section className="section nginx-modal log-viewer">
      <div className="section-title">
        <div className="nginx-config-title">
          <h2>Webserver logs - {logViewer.domain}</h2>
          <p className="hint">{logViewer.path || `/var/log/nginx/${logViewer.domain}.${logViewer.kind}.log`}</p>
        </div>
        <button className="secondary-light" onClick={() => setLogViewer(null)}><X size={14}/> Close</button>
      </div>
      <div className="log-toolbar">
        <div className="segmented-control">
          <button className={logViewer.kind === 'access' ? 'active' : ''} disabled={!!loading} onClick={() => loadWebsiteLog(logViewer.id, 'access', logViewer.lines, logViewer.domain)}>Access</button>
          <button className={logViewer.kind === 'error' ? 'active' : ''} disabled={!!loading} onClick={() => loadWebsiteLog(logViewer.id, 'error', logViewer.lines, logViewer.domain)}>Errors (PHP + server)</button>
        </div>
        <select value={logViewer.lines} onChange={e => loadWebsiteLog(logViewer.id, logViewer.kind, Number(e.target.value), logViewer.domain)} disabled={!!loading}>
          <option value={100}>100 lines</option>
          <option value={200}>200 lines</option>
          <option value={500}>500 lines</option>
          <option value={1000}>1000 lines</option>
          <option value={2000}>2000 lines</option>
        </select>
        <button disabled={!!loading} onClick={() => loadWebsiteLog(logViewer.id, logViewer.kind, logViewer.lines, logViewer.domain)}><RefreshCw size={14}/> Refresh</button>
      </div>
      <pre className="log-output">{logViewer.exists
        ? (logViewer.content || 'Log is empty.')
        : (logViewer.kind === 'error' ? 'No errors logged yet.' : 'Log file has not been created yet.')}</pre>
    </section>;
  }

  function renderWebsites() {
    const wpFieldsEnabled = siteType === 'wordpress' && installWordPress;
    const wsQuery = websiteSearch.trim().toLowerCase();
    const filteredWebsites = wsQuery
      ? websites.filter(s => (s.domain || '').toLowerCase().includes(wsQuery)
          || (s.aliases || []).some(a => (a.domain || '').toLowerCase().includes(wsQuery)))
      : websites;
    return <>
      <section className="section">
        <h2>Create website</h2>
        <div className="form-row create-site-row">
          <input id="create-website-domain" value={domain} onChange={e => setDomain(e.target.value)} placeholder="domain.com" />
          <select value={siteType} onChange={e => setSiteType(e.target.value)}>
            <option value="wordpress">WordPress</option>
            <option value="php">PHP</option>
          </select>
          <select value={phpVersion} onChange={e => setPhpVersion(e.target.value)}>
            {phpVersionOptions(phpVersions.installed, phpVersion).map(v => <option key={v} value={v}>PHP {v}</option>)}
          </select>
          {wpFieldsEnabled && <input value={adminEmail} onChange={e => setAdminEmail(e.target.value)} placeholder="admin@domain.com" />}
          {wpFieldsEnabled && <input value={wpAdminUser} onChange={e => setWpAdminUser(e.target.value)} placeholder="WP admin user" />}
          {wpFieldsEnabled && <input value={wpAdminPassword} onChange={e => setWpAdminPassword(e.target.value)} placeholder="WP admin password" type="password" />}
          <button disabled={!!loading || !domain} onClick={createWordPress}><Plus size={15}/> Create</button>
        </div>
        {siteType === 'wordpress' && <label className="check-line">
          <input type="checkbox" checked={installWordPress} onChange={e => setInstallWordPress(e.target.checked)} />
          Install WordPress (creates database, downloads WP, configures vhost)
        </label>}
        <label className="check-line">
          <input type="checkbox" checked={installSslAfterCreate} onChange={e => setInstallSslAfterCreate(e.target.checked)} />
          Set up SSL after creating
        </label>
        {installSslAfterCreate && <div className="create-ssl-box">
          <div className="segmented ssl-mode-tabs">
            <button className={createSslMode === 'letsencrypt' ? 'active' : ''} onClick={() => setCreateSslMode('letsencrypt')}><Lock size={13}/> Let's Encrypt</button>
            <button className={createSslMode === 'wildcard' ? 'active' : ''} onClick={() => setCreateSslMode('wildcard')}><Globe size={13}/> Wildcard (Cloudflare)</button>
            <button className={createSslMode === 'existing' ? 'active' : ''} onClick={() => { setCreateSslMode('existing'); loadCreateSslCerts(); }}><RefreshCw size={13}/> Use existing</button>
            <button className={createSslMode === 'manual' ? 'active' : ''} onClick={() => setCreateSslMode('manual')}><KeyRound size={13}/> Manual</button>
          </div>
          {createSslMode === 'letsencrypt' && <p className="hint">certbot HTTP-01 — the domain must point to this server's IP first.</p>}
          {createSslMode === 'wildcard' && <div className="create-ssl-fields">
            <input type="password" autoComplete="off" placeholder="Cloudflare API Token" value={createSslForm.api_token} onChange={e => setCreateSslForm(p => ({ ...p, api_token: e.target.value }))} />
            <input type="email" autoComplete="off" placeholder="Contact email (optional)" value={createSslForm.email} onChange={e => setCreateSslForm(p => ({ ...p, email: e.target.value }))} />
            <p className="hint">A scoped <strong>API Token</strong> (My Profile → API Tokens → Create Token), <strong>not</strong> the Global API Key. Permission <code>Zone → DNS → Edit</code> for the zone(s) you issue certs for. Stored encrypted for renewal. Issues one cert for the domain and *.domain — no DNS record or port 80 needed.</p>
          </div>}
          {createSslMode === 'existing' && <div className="create-ssl-fields">
            <div className="form-row">
              <select value={createSslForm.reuse_name} onChange={e => setCreateSslForm(p => ({ ...p, reuse_name: e.target.value }))}>
                <option value="">— pick a certificate on this server —</option>
                {createSslCerts.map(c => <option key={c.name} value={c.name}>{c.domains.join(', ')} ({c.source})</option>)}
              </select>
              <button className="secondary-light" type="button" onClick={loadCreateSslCerts}><RefreshCw size={13}/> Refresh</button>
            </div>
            <p className="hint">{!domain.trim()
              ? 'Type the domain above first — this list shows only certificates that cover it.'
              : createSslCerts.length === 0
                ? `No certificate on this server covers ${domain.trim().toLowerCase()}. Note a *.example.com wildcard covers x.example.com but not example.com itself. Issue a Let's Encrypt or wildcard cert first.`
                : 'A *.example.com wildcard issued earlier covers every x.example.com.'}</p>
          </div>}
          {createSslMode === 'manual' && <div className="create-ssl-fields">
            <textarea rows={4} placeholder="-----BEGIN CERTIFICATE-----" value={createSslForm.certificate} onChange={e => setCreateSslForm(p => ({ ...p, certificate: e.target.value }))} />
            <textarea rows={4} placeholder="-----BEGIN PRIVATE KEY-----" value={createSslForm.private_key} onChange={e => setCreateSslForm(p => ({ ...p, private_key: e.target.value }))} />
            <textarea rows={3} placeholder="CA bundle (optional)" value={createSslForm.ca_bundle} onChange={e => setCreateSslForm(p => ({ ...p, ca_bundle: e.target.value }))} />
          </div>}
        </div>}
        <p className="hint">{wpFieldsEnabled
          ? 'WordPress will be installed and the panel will show the URL, admin account, and password after creation.'
          : 'A virtual host will be created with public_html/ folder. Upload your PHP, HTML, or static files via File Manager.'}</p>
      </section>
      <section className="section">
        <div className="section-title">
          <h2>Website list</h2>
          <button disabled={!!loading} onClick={refreshAll}><RefreshCw size={15}/> Refresh</button>
        </div>
        {websites.length > 0 && <div className="website-search">
          <Search size={15}/>
          <input value={websiteSearch} onChange={e => setWebsiteSearch(e.target.value)} placeholder="Filter domains…" />
          {websiteSearch && <button className="mini secondary-light" onClick={() => setWebsiteSearch('')}><X size={13}/></button>}
          <span className="hint">{wsQuery ? `${filteredWebsites.length} of ${websites.length}` : `${websites.length} website${websites.length === 1 ? '' : 's'}`}</span>
        </div>}
        {websites.length === 0 && <EmptyState icon={Globe} message="No websites yet." action={{ label: 'New website', icon: Plus, onClick: () => { const el = document.getElementById('create-website-domain'); el?.scrollIntoView({ behavior: 'smooth', block: 'center' }); el?.focus(); } }} />}
        {websites.length > 0 && filteredWebsites.length === 0 && <EmptyState icon={Globe} message={`No domain matches “${websiteSearch}”.`} />}
        <div className="site-grid">
          {filteredWebsites.map(site => <div className="site-stack" key={site.id}>
          <article className="site-card">
            <div className="site-head">
              <div>
                <a className="site-link" href={websiteUrl(site)} target="_blank" rel="noopener noreferrer">{site.domain}</a>
                <small>{site.root_path}</small>
              </div>
            </div>
            <div className="site-meta">
              <span className={`badge site-ssl-badge ${site.ssl_enabled ? 'ok' : ''}`}>{site.ssl_wildcard ? 'Wildcard' : site.ssl_mode === 'reuse' ? 'Shared cert' : site.ssl_enabled ? 'SSL OK' : 'No SSL'}</span>
              <span>Type <strong>{site.app_type || 'wordpress'}</strong></span>
              <span>PHP <strong>{site.php_version}</strong></span>
              {site.app_type === 'php' && site.nginx_rewrite_mode && site.nginx_rewrite_mode !== 'none' && <span>Rewrite <strong>{site.nginx_rewrite_mode}</strong></span>}
              {site.waf_enabled && <span className="badge ok">WAF</span>}
              {(site.aliases || []).length > 0 && <span>Domains <strong>{(site.aliases || []).length + 1}</strong></span>}
            </div>
            <div className="site-actions" aria-label={`Website actions for ${site.domain}`}>
              <div className="site-feature-actions">
                <button className="site-icon-button secondary-light" data-tooltip="Files" title="Files" aria-label={`Open file manager for ${site.domain}`} disabled={!!loading} onClick={() => openWebsiteFileManager(site)}><FolderOpen size={15}/></button>
                <button className="site-icon-button secondary-light" data-tooltip="Logs" title="Logs" aria-label={`View logs for ${site.domain}`} disabled={!!loading} onClick={() => openWebsiteLogs(site)}><FileText size={15}/></button>
                <button className="site-icon-button secondary-light" data-tooltip="Terminal" title="Terminal" aria-label={`Open terminal for ${site.domain}`} disabled={!!loading} onClick={() => openWebsiteTerminal(site)}><TerminalIcon size={15}/></button>
                <button className="site-icon-button secondary-light" data-tooltip="Settings" title="Settings" aria-label={`Edit settings for ${site.domain}`} disabled={!!loading} onClick={() => openNginxCustom(site)}><SettingsIcon size={15}/></button>
                {!site.wp_installed && <button className="site-icon-button secondary-light" data-tooltip="Install WP" title="Install WordPress" aria-label={`Install WordPress on ${site.domain}`} disabled={!!loading} onClick={() => openWpInstall(site)}><Download size={15}/></button>}
                {site.wp_installed && <button className="site-icon-button secondary-light" data-tooltip="Update WP" title="Update WordPress" aria-label={`Update WordPress on ${site.domain}`} disabled={!!loading} onClick={() => openWpUpdate(site)}><RefreshCw size={15}/></button>}
                <button className="site-icon-button danger" data-tooltip="Delete" title="Delete" aria-label={`Delete ${site.domain}`} disabled={!!loading} onClick={() => deleteWebsite(site.id)}><Trash2 size={15}/></button>
              </div>
            </div>
          </article>
          {nginxCustomEditing?.id === site.id && renderNginxEditor()}
          {logViewer?.id === site.id && renderWebsiteLogViewer()}
          {terminalViewer?.id === site.id && renderWebsiteTerminal()}
          {wpManagerSite?.id === site.id && <div className="wp-manager-panel">
            <div className="user-edit-heading">
              <strong>{wpManagerMode === 'install' ? `Install WordPress on ${site.domain}` : `Update WordPress on ${site.domain}`}</strong>
              <button className="user-edit-close secondary-light" onClick={() => setWpManagerSite(null)}><X size={16}/></button>
            </div>
            {wpManagerMode === 'install' ? <div className="user-edit-grid">
              <p className="hint">Creates a database, downloads WordPress, configures vhost.</p>
              <label><span>Site title</span><input value={wpInstallForm.title} onChange={e => setWpInstallForm(prev => ({ ...prev, title: e.target.value }))} /></label>
              <label><span>Admin username</span><input value={wpInstallForm.admin_user} onChange={e => setWpInstallForm(prev => ({ ...prev, admin_user: e.target.value }))} /></label>
              <label><span>Admin email</span><input type="email" value={wpInstallForm.admin_email} onChange={e => setWpInstallForm(prev => ({ ...prev, admin_email: e.target.value }))} /></label>
              <label><span>Admin password</span><input type="password" value={wpInstallForm.admin_password} onChange={e => setWpInstallForm(prev => ({ ...prev, admin_password: e.target.value }))} placeholder="Min 12 characters" /></label>
              <div className="user-edit-actions">
                <button className="secondary-light" onClick={() => setWpManagerSite(null)}>Cancel</button>
                <button disabled={!!loading || !wpInstallForm.admin_password || wpInstallForm.admin_password.length < 12} onClick={installWpOnSite}><Download size={14}/> Install WordPress</button>
              </div>
            </div> : <div>
              <p className="hint">Updates WordPress core, all plugins, and all themes to the latest version.</p>
              <div className="user-edit-actions">
                <button className="secondary-light" onClick={() => setWpManagerSite(null)}>Cancel</button>
                <button disabled={!!loading} onClick={updateWpOnSite}><RefreshCw size={14}/> Update All</button>
              </div>
            </div>}
          </div>}
          </div>)}
        </div>
      </section>
    </>;
  }

  function renderSsl() {
    const sslLabel = currentSite?.ssl_mode === 'manual'
      ? 'Manual SSL'
      : currentSite?.ssl_mode === 'reuse'
        ? 'Existing certificate'
        : currentSite?.ssl_wildcard
          ? 'Wildcard SSL'
          : currentSite?.ssl_enabled
            ? 'SSL Enabled'
            : 'SSL Disabled';
    const sslUpdated = currentSite?.ssl_updated_at ? new Date(currentSite.ssl_updated_at).toLocaleString() : '';
    return <section className="section">
      <h2>SSL Certificate</h2>
      <WebsiteSelect />
      {currentSite && <div className="info-box" style={{marginTop:8}}>
        <strong>{currentSite.domain}</strong>
        <span className={currentSite.ssl_enabled ? 'badge ok' : 'badge'} style={{justifySelf:'start'}}>{sslLabel}</span>
        {currentSite.ssl_wildcard && <span className="badge ok" style={{justifySelf:'start'}}>*.{currentSite.domain}</span>}
        {currentSite.ssl_mode === 'reuse' && currentSite.ssl_reuse_name && <span className="hint">{currentSite.ssl_reuse_name.split(':')[1]}</span>}
        {sslUpdated && <span className="hint">Updated {sslUpdated}</span>}
        {currentSite.ssl_mode === 'manual' && currentSite.ssl_has_ca && <span className="badge ok" style={{justifySelf:'start'}}>CA Bundle</span>}
      </div>}
      <div className="segmented ssl-mode-tabs">
        <button className={sslMode === 'letsencrypt' ? 'active' : ''} onClick={() => setSslMode('letsencrypt')}><Lock size={14}/> Let's Encrypt</button>
        <button className={sslMode === 'wildcard' ? 'active' : ''} onClick={() => setSslMode('wildcard')}><Globe size={14}/> Wildcard (Cloudflare)</button>
        <button className={sslMode === 'existing' ? 'active' : ''} onClick={() => { setSslMode('existing'); loadAvailableCerts(); }}><RefreshCw size={14}/> Use existing</button>
        <button className={sslMode === 'manual' ? 'active' : ''} onClick={() => setSslMode('manual')}><KeyRound size={14}/> Manual SSL</button>
      </div>
      {sslMode === 'letsencrypt' ? <>
        <button disabled={!selectedWebsiteId || !!loading} onClick={() => enableSsl(selectedWebsiteId)} style={{marginTop:8}}><Lock size={15}/> Install / Renew SSL</button>
        <p className="hint">certbot HTTP-01 — the domain must point to this server's IP before issuing.</p>
      </> : sslMode === 'wildcard' ? <div className="manual-ssl-grid">
        <label style={{gridColumn:'1 / -1'}}>Cloudflare API Token
          <input type="password" autoComplete="off" value={wildcardSslForm.api_token} onChange={e => setWildcardSslForm(p => ({ ...p, api_token: e.target.value }))} placeholder={currentSite?.ssl_wildcard ? 'Stored — leave blank to reuse' : 'Scoped API Token (not the Global API Key)'} />
        </label>
        <label style={{gridColumn:'1 / -1'}}>Contact email (optional)
          <input type="email" autoComplete="off" value={wildcardSslForm.email} onChange={e => setWildcardSslForm(p => ({ ...p, email: e.target.value }))} placeholder="admin@domain.com" />
        </label>
        <button className="manual-ssl-submit" disabled={!selectedWebsiteId || !!loading} onClick={issueWildcardSsl}><Globe size={15}/> {currentSite?.ssl_wildcard ? 'Renew / re-issue wildcard' : 'Issue wildcard certificate'}</button>
        <p className="hint" style={{gridColumn:'1 / -1'}}>Use a <strong>scoped API Token</strong> from Cloudflare (My Profile → API Tokens → Create Token → permission <code>Zone → DNS → Edit</code> for the zone), <strong>not</strong> the account Global API Key. It is stored encrypted and re-used for automatic renewal. Issues one cert for <strong>{currentSite?.domain || 'the domain'}</strong> and <strong>*.{currentSite?.domain || 'domain'}</strong> via a Cloudflare DNS challenge — no DNS record or port 80 needed. Other websites can then pick this cert under "Use existing".</p>
      </div> : sslMode === 'existing' ? <div className="manual-ssl-grid">
        <div className="form-row" style={{gridColumn:'1 / -1'}}>
          <select value={reuseCertName} onChange={e => setReuseCertName(e.target.value)}>
            <option value="">— pick a certificate on this server —</option>
            {availableCerts.map(c => <option key={c.name} value={c.name} disabled={!c.covers_domain}>
              {c.domains.join(', ')} · {c.source}{c.covers_domain ? '' : ' (does not cover this domain)'}
            </option>)}
          </select>
          <button className="secondary-light" type="button" disabled={!!loading} onClick={loadAvailableCerts}><RefreshCw size={13}/> Refresh</button>
        </div>
        <button className="manual-ssl-submit" disabled={!selectedWebsiteId || !reuseCertName || !!loading} onClick={reuseExistingSsl}><Lock size={15}/> Use this certificate</button>
        <p className="hint" style={{gridColumn:'1 / -1'}}>{availableCerts.length === 0
          ? 'No certificate on this server. Issue a Let’s Encrypt or wildcard cert first (here or on another domain).'
          : 'A *.example.com wildcard covers every x.example.com — issue it once, reuse it everywhere.'}</p>
      </div> : <div className="manual-ssl-grid">
        <label>
          Certificate (.crt/.pem)
          <input type="file" accept=".crt,.pem" onChange={e => setManualSslFiles(prev => ({ ...prev, certificate: e.target.files?.[0] || null }))} />
        </label>
        <label>
          Private key (.key/.pem)
          <input type="file" accept=".key,.pem" onChange={e => setManualSslFiles(prev => ({ ...prev, private_key: e.target.files?.[0] || null }))} />
        </label>
        <label>
          CA bundle (.ca/.crt/.pem)
          <input type="file" accept=".ca,.crt,.pem" onChange={e => setManualSslFiles(prev => ({ ...prev, ca_bundle: e.target.files?.[0] || null }))} />
        </label>
        <textarea rows={7} disabled={!!manualSslFiles.certificate} value={manualSslForm.certificate} onChange={e => setManualSslForm(prev => ({ ...prev, certificate: e.target.value }))} placeholder="-----BEGIN CERTIFICATE-----" />
        <textarea rows={7} disabled={!!manualSslFiles.private_key} value={manualSslForm.private_key} onChange={e => setManualSslForm(prev => ({ ...prev, private_key: e.target.value }))} placeholder="-----BEGIN PRIVATE KEY-----" />
        <textarea rows={7} disabled={!!manualSslFiles.ca_bundle} value={manualSslForm.ca_bundle} onChange={e => setManualSslForm(prev => ({ ...prev, ca_bundle: e.target.value }))} placeholder="Optional CA bundle" />
        <button className="manual-ssl-submit" disabled={!selectedWebsiteId || !!loading} onClick={installManualSsl}><Upload size={15}/> Install Manual SSL</button>
      </div>}
    </section>;
  }

  function renderDatabases() {
    const dbQuery = databaseSearch.trim().toLowerCase();
    const websiteById = new Map(websites.map(w => [w.id, w.domain]));
    const filteredDatabases = dbQuery
      ? databases.filter(db => (db.db_name || '').toLowerCase().includes(dbQuery)
          || (db.db_user || '').toLowerCase().includes(dbQuery)
          || (websiteById.get(db.website_id) || '').toLowerCase().includes(dbQuery))
      : databases;
    function copyToClipboard(text, field) {
      const doCopy = navigator.clipboard ? navigator.clipboard.writeText(text) : new Promise((resolve, reject) => {
        try { const ta = document.createElement('textarea'); ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0'; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta); resolve(); } catch(e) { reject(e); }
      });
      doCopy.then(() => { setCopiedField(field); setTimeout(() => setCopiedField(null), 2000); }).catch(() => setError('Copy failed.'));
    }
    return <section className="section">
      <div className="section-title">
        <h2>Databases</h2>
        <button disabled={!!loading} onClick={refreshAll}><RefreshCw size={15}/> Refresh</button>
      </div>
      <div className="form-row">
        <input id="create-database-name" value={newDatabase.db_name} onChange={e => setNewDatabase(prev => ({ ...prev, db_name: e.target.value }))} placeholder="database_name" />
        <input value={newDatabase.db_user} onChange={e => setNewDatabase(prev => ({ ...prev, db_user: e.target.value }))} placeholder="db_user (default = db_name)" />
        <input value={newDatabase.db_password} onChange={e => setNewDatabase(prev => ({ ...prev, db_password: e.target.value }))} placeholder="password (min 12 chars)" />
        <button className="mini secondary-light" title="Generate random password" onClick={() => setNewDatabase(prev => ({ ...prev, db_password: generateRandomPassword() }))}><Dices size={13}/></button>
        <button disabled={!!loading || !newDatabase.db_name.trim()} onClick={createDatabase}><Plus size={15}/> Create database</button>
      </div>
      {createdDbInfo && <div className="info-box db-created-box">
        <div className="db-created-head"><strong>Database created successfully</strong><button className="mini secondary-light" onClick={() => setCreatedDbInfo(null)}><X size={13}/></button></div>
        <div className="db-created-grid">
          <label>Database</label><span>{createdDbInfo.db_name} <button className="mini secondary-light" title={copiedField === 'db_name' ? 'Copied!' : 'Copy'} onClick={() => copyToClipboard(createdDbInfo.db_name, 'db_name')}>{copiedField === 'db_name' ? <Check size={12} style={{color:'var(--success)'}}/> : <Copy size={12}/>}</button></span>
          <label>User</label><span>{createdDbInfo.db_user} <button className="mini secondary-light" title={copiedField === 'db_user' ? 'Copied!' : 'Copy'} onClick={() => copyToClipboard(createdDbInfo.db_user, 'db_user')}>{copiedField === 'db_user' ? <Check size={12} style={{color:'var(--success)'}}/> : <Copy size={12}/>}</button></span>
          <label>Password</label><span><code>{createdDbInfo.db_password}</code> <button className="mini secondary-light" title={copiedField === 'db_password' ? 'Copied!' : 'Copy'} onClick={() => copyToClipboard(createdDbInfo.db_password, 'db_password')}>{copiedField === 'db_password' ? <Check size={12} style={{color:'var(--success)'}}/> : <Copy size={12}/>}</button></span>
        </div>
      </div>}
      {databases.length === 0 && !createdDbInfo && <EmptyState icon={Database} message="No databases found." action={{ label: 'New database', icon: Plus, onClick: () => { const el = document.getElementById('create-database-name'); el?.scrollIntoView({ behavior: 'smooth', block: 'center' }); el?.focus(); } }} />}
      {databases.length > 0 && <div className="list-search">
        <Search size={15}/>
        <input value={databaseSearch} onChange={e => setDatabaseSearch(e.target.value)} placeholder="Filter databases…" />
        {databaseSearch && <button className="mini secondary-light" onClick={() => setDatabaseSearch('')}><X size={13}/></button>}
        <span className="hint">{dbQuery ? `${filteredDatabases.length} of ${databases.length}` : `${databases.length} database${databases.length === 1 ? '' : 's'}`}</span>
      </div>}
      {databases.length > 0 && filteredDatabases.length === 0 && <EmptyState icon={Database} message={`No database matches “${databaseSearch}”.`} />}
      <div className="table">
        {filteredDatabases.map(db => {
          return <div className="row db-row" key={db.id}>
          <span><strong>{db.db_name}</strong></span>
          <span style={{color:'var(--text-muted)'}}>{db.db_user}</span>
          <button disabled={!!loading} onClick={() => openPhpMyAdmin(db.id)}>phpMyAdmin</button>
          <button disabled={!!loading} onClick={() => downloadDatabase(db.id, db.db_name)}><Download size={14}/> SQL</button>
          <button disabled={!!loading} onClick={() => changeDbPassword(db.id)}><KeyRound size={14}/> Password</button>
          <button className="danger" disabled={!!loading} onClick={() => deleteDatabase(db.id, db.db_name)}><Trash2 size={14}/></button>
        </div>})}
      </div>
      <p className="hint">Click phpMyAdmin to sign in directly. Token expires after 60s.</p>
    </section>;
  }

  function renderCron() {
    return <section className="section">
      <div className="section-title">
        <div><h2>Cron manager</h2></div>
        <button disabled={!selectedWebsiteId || !!loading} onClick={listCron}><RefreshCw size={14}/> Refresh</button>
      </div>
      <div className="cron-form">
        <WebsiteSelect />
        <input value={cronSchedule} onChange={e => setCronSchedule(e.target.value)} placeholder="*/15 * * * *" />
        <input value={cronCommand} onChange={e => setCronCommand(e.target.value)} placeholder="wget -q -O - https://example.com/wp-cron.php" />
        <button disabled={!selectedWebsiteId || !!loading} onClick={addCron}><Plus size={14}/> Add cron</button>
      </div>
      {selectedWebsiteId && <p className="hint">Cron runs as <strong>{cronUser || currentSite?.linux_user || 'www-data'}</strong> for the selected website. Accepted commands: <code>wget</code>/<code>curl</code> to an http(s) URL, WP-CLI maintenance commands, or a <code>.php</code> file inside this website. Output is discarded automatically.</p>}
      <div className="cron-list">
        {selectedWebsiteId && cronItems.length === 0 && <EmptyState icon={Clock} message="No cron jobs found for this website." />}
        {cronItems.map(item => <div className="cron-item" key={`${item.index}-${item.line}`}>
          <span className="badge">#{item.index}</span>
          <span><strong>{item.schedule}</strong><small>{item.command || item.line}</small></span>
          <button className="mini danger" disabled={!!loading} onClick={() => deleteCron(item.index)}><Trash2 size={13}/></button>
        </div>)}
      </div>
    </section>;
  }

  function renderFiles() {
    const allSelected = files.length > 0 && selectedFilePaths.length === files.length;
    const visibleFileJobs = fileJobs.filter(job => String(job.website_id) === String(selectedWebsiteId)).slice(0, 4);
    const selectedArchive = selectedFilePaths.length === 1
      ? files.find(item => item.path === selectedFilePaths[0] && isExtractableArchive(item))
      : null;
    return <section className="section">
      <div className="section-title">
        <div><h2>File manager</h2></div>
        <button disabled={!selectedWebsiteId || !!loading} onClick={() => listFiles(fileListPath)}><RefreshCw size={14}/> Refresh</button>
      </div>
      <div className="file-manager">
        <div className="file-panel">
          <div className="file-controls">
            <WebsiteSelect />
            {currentSite && <div className="file-meta">
              <span>Website: <strong>{currentSite.domain}</strong></span>
              <span>Root: <strong>{currentSite.root_path}{fileListPath ? `/${fileListPath}` : ''}</strong></span>
              {currentUser && !isAdmin && <span>Storage: <strong>{storageUsageText(currentUser)}</strong></span>}
            </div>}
            <div className="path-pill breadcrumb-line">
              <button className="crumb" disabled={!selectedWebsiteId || fileListPath === ''} onClick={() => listFiles('')}>root</button>
              {fileBreadcrumbs(fileListPath).map(crumb => <button className="crumb" key={crumb.path} onClick={() => listFiles(crumb.path)}>{crumb.label}</button>)}
            </div>
            <div className="file-toolbar">
              <button disabled={!selectedWebsiteId || fileListPath === '' || !!loading} onClick={() => listFiles(parentFilePath(fileListPath))}>Up</button>
              <button disabled={!selectedWebsiteId || !!loading} onClick={makeFileDirectory}><Plus size={14}/> Folder</button>
              <button disabled={!selectedWebsiteId || !!loading} onClick={makeFile}><FileText size={14}/> File</button>
              <label className={`upload-button ${(!selectedWebsiteId || !!loading) ? 'disabled' : ''}`}>
                <Upload size={14}/> Upload
                <input type="file" disabled={!selectedWebsiteId || !!loading} onChange={e => { uploadSiteFile(e.target.files?.[0]); e.target.value = ''; }} />
              </label>
              <select value={archiveFormat} onChange={e => setArchiveFormat(e.target.value)} disabled={!selectedWebsiteId || !!loading}>
                <option value="zip">zip</option>
                <option value="tar.gz">tar.gz</option>
              </select>
              <button disabled={selectedFilePaths.length === 0 || !!loading} onClick={copySelectedFiles}><Copy size={14}/> Copy</button>
              <button disabled={selectedFilePaths.length === 0 || !!loading} onClick={moveSelectedFiles}><MoveRight size={14}/> Move</button>
              <button disabled={selectedFilePaths.length === 0 || !!loading} onClick={archiveSelectedFiles}><Archive size={14}/> Archive</button>
              <button disabled={!selectedArchive || !!loading} onClick={extractSelectedArchive}><PackageOpen size={14}/> Extract</button>
              <button className="danger" disabled={selectedFilePaths.length === 0 || !!loading} onClick={deleteSelectedFiles}><Trash2 size={14}/> Delete</button>
            </div>
            {visibleFileJobs.length > 0 && <div className="file-job-list">
              {visibleFileJobs.map(job => <div className={`file-job ${job.status}`} key={job.job_id}>
                <Clock size={14}/>
                <span><strong>{job.archive_path?.split('/').pop() || 'Archive'}</strong> {job.status === 'done' ? 'completed' : job.status === 'error' ? 'failed' : job.status}</span>
                {job.error && <small>{job.error}</small>}
              </div>)}
            </div>}
          </div>
          <div className="file-list-header">
            <label><input type="checkbox" checked={allSelected} onChange={toggleAllFiles} disabled={files.length === 0} /> Select</label>
            <span>{files.length} item(s)</span>
          </div>
          <div className="file-list">
            {files.length === 0 && <div className="empty-box">No files in this folder.</div>}
            {files.map(item => <div className={`file-item ${selectedFilePaths.includes(item.path) ? 'selected' : ''}`} key={item.path}>
              <input type="checkbox" checked={selectedFilePaths.includes(item.path)} onChange={() => toggleFileSelection(item.path)} />
              <button className="file-name" onClick={() => item.is_dir ? listFiles(item.path) : (isTextEditable(item) ? openFileEditorTab(item.path) : downloadFile(item.path))}>
                {item.is_dir ? <FolderOpen size={16}/> : <FileText size={16}/>} <strong>{item.name}</strong>
              </button>
              <span className="file-mode">{item.mode || '---'}</span>
              <span className="file-size">{item.is_dir ? 'Folder' : formatBytes(item.size)}</span>
              <div className="file-row-actions">
                {!item.is_dir && <button className="mini secondary-light" disabled={!!loading} onClick={() => downloadFile(item.path)}><Download size={13}/></button>}
                {isExtractableArchive(item) && <button className="mini secondary-light" disabled={!!loading} onClick={() => extractArchive(item)}><PackageOpen size={13}/> Extract</button>}
                <button className="mini secondary-light" disabled={!!loading} onClick={() => renameFileItem(item)}>Rename</button>
              </div>
            </div>)}
          </div>
        </div>
      </div>
    </section>;
  }

  function renderBackups() {
    const selectedBackupUser = users.find(user => String(user.id) === String(selectedBackupUserId));
    const userNameById = id => users.find(user => String(user.id) === String(id))?.username || `User #${id}`;
    const scheduleUserLabel = item => {
      if (item.all_users) return 'All users';
      const ids = (item.user_ids && item.user_ids.length > 0) ? item.user_ids : (item.user_id ? [item.user_id] : []);
      return ids.length ? ids.map(userNameById).join(', ') : 'No users';
    };
    const jobTitle = job => ({ site_backup: 'Website backup', user_backup: 'Full user backup', sftp_backup: 'SFTP backup' }[job.kind] || 'Backup task');
    const jobDetail = job => job.error || job.remote_file || job.backup_file || job.message || job.status;
    const jobTimestamp = job => {
      const stamp = job.finished_at || job.started_at || job.created_at || '';
      if (!stamp) return 'No timestamp';
      const date = new Date(stamp);
      return Number.isNaN(date.getTime()) ? stamp : new Intl.DateTimeFormat('en-GB', {
        timeZone: 'Asia/Ho_Chi_Minh',
        hour12: false,
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        day: '2-digit',
        month: '2-digit',
        year: 'numeric',
      }).format(date).replace(',', '');
    };
    const backupTabs = isAdmin
      ? [
        ['website', 'Backup website', Globe],
        ['user', 'Backup user', Users],
        ['da-import', 'Import DA Backups', Download],
        ['schedule', 'Scheduled backups', Clock],
        ['destination', 'Backup Destination', Network],
        ['logs', 'Backup logs', FileText],
      ]
      : [
        ['website', 'Backup website', Globe],
        ['logs', 'Backup logs', FileText],
      ];
    const activeBackupTab = backupTabs.some(([id]) => id === backupTab) ? backupTab : 'website';

    return <section className="section backups-page">
      <h2>Backups</h2>
      <div className="segmented-control backup-tabs" role="tablist" aria-label="Backup sections">
        {backupTabs.map(([id, label, Icon]) => <button
          key={id}
          type="button"
          role="tab"
          aria-selected={activeBackupTab === id}
          className={activeBackupTab === id ? 'active' : ''}
          onClick={() => setBackupTab(id)}
        ><Icon size={14}/>{label}</button>)}
      </div>

      {activeBackupTab === 'logs' && <div className="backup-tab-panel backup-logs-panel">
        <div className="backup-panel-title">
          <div><h3>Backup logs</h3><p className="hint">Recent queued, running, completed, and failed backup tasks.</p></div>
          <button disabled={!!loading} onClick={() => loadBackupJobs(true)}><RefreshCw size={14}/> Refresh</button>
        </div>
        {backupJobs.length === 0 && <EmptyState icon={FileText} message="No backup logs found." />}
        {backupJobs.length > 0 && <div className="backup-job-list">
          {backupJobs.map(job => <div className={`backup-job ${job.status}`} key={job.job_id}>
            <Clock size={14}/>
            <span>
              <strong>{jobTitle(job)}</strong>
              <small>{jobDetail(job)}</small>
              <small className="backup-job-time">{jobTimestamp(job)}</small>
            </span>
            <span className={job.status === 'done' ? 'badge ok' : job.status === 'error' ? 'badge bad' : 'badge'}>{job.status}</span>
          </div>)}
        </div>}
      </div>}

      {activeBackupTab === 'website' && <div className="backup-tab-panel">
        <div className="backup-panel-title">
          <div><h3>Backup website</h3><p className="hint">Backups include website source files and a database SQL export.</p></div>
        </div>
        <WebsiteSelect />
        <div className="actions backup-toolbar">
          <button disabled={!selectedWebsiteId || !!loading} onClick={createBackup}><Plus size={14}/> Create backup</button>
          <button disabled={!selectedWebsiteId || !!loading} onClick={refreshBackupArea}><RefreshCw size={14}/> Refresh</button>
          <label className="upload-button">
            <Upload size={14}/> Upload backup
            <input type="file" accept=".tar.gz,application/gzip" onChange={e => { uploadBackup(e.target.files?.[0]); e.target.value = ''; }} />
          </label>
        </div>
        {backups.length === 0 && selectedWebsiteId && <EmptyState icon={Archive} message="No backups found for this website." action={{ label: 'Create backup', icon: Plus, onClick: () => { if (!loading) createBackup(); } }} />}
        <div className="backup-list">
          {backups.map(file => <div className="backup-item" key={file}>
            <span>{file.split('/').pop()}</span>
            <div className="actions">
              <button disabled={!!loading} onClick={() => downloadBackup(file)}><Download size={14}/> Download</button>
              <button disabled={!!loading} onClick={() => restoreBackup(file)}><RotateCcw size={14}/> Restore</button>
              <button className="danger" disabled={!!loading} onClick={() => deleteBackup(file)}><Trash2 size={14}/></button>
            </div>
          </div>)}
        </div>
      </div>}

      {isAdmin && activeBackupTab === 'user' && <div className="backup-tab-panel">
        <div className="backup-panel-title">
          <div><h3>Backup user</h3><p className="hint">Includes the panel user, all owned websites, source files, database dumps, and restore metadata.</p></div>
          <button disabled={!!loading} onClick={refreshUserBackupArea}><RefreshCw size={14}/> Reload</button>
        </div>
        <div className="sftp-run-row user-backup-row backup-run-row">
          <select value={selectedBackupUserId} onChange={e => setSelectedBackupUserId(e.target.value)}>
            <option value="">Select user</option>
            {users.map(user => <option key={user.id} value={user.id}>{user.username}</option>)}
          </select>
          <select value={selectedSftpTargetId} onChange={e => setSelectedSftpTargetId(e.target.value)}>
            <option value="">Local only</option>
            {sftpTargets.map(target => <option key={target.id} value={target.id}>{target.name}</option>)}
          </select>
          <button disabled={!selectedBackupUserId || !!loading} onClick={createUserBackup}><Archive size={14}/> Create backup</button>
        </div>
        {selectedBackupUser && <p className="hint">Current user: <strong>{selectedBackupUser.username}</strong></p>}
        <div className="actions backup-subactions">
          <button disabled={!selectedBackupUserId || !!loading} onClick={() => listUserBackups()}><RefreshCw size={14}/> Refresh list</button>
        </div>
        {selectedBackupUserId && userBackups.length === 0 && <EmptyState icon={Archive} message="No user backups found." />}
        <div className="backup-list">
          {userBackups.map(file => <div className="backup-item" key={file}>
            <span>{file.split('/').pop()}</span>
            <div className="actions">
              <button disabled={!!loading} onClick={() => downloadUserBackup(file)}><Download size={14}/> Download</button>
              <button disabled={!!loading} onClick={() => restoreUserBackup(file)}><RotateCcw size={14}/> Restore user</button>
              <button className="danger" disabled={!!loading} onClick={() => deleteUserBackup(file)}><Trash2 size={14}/></button>
            </div>
          </div>)}
        </div>

        <div className="section-title restore-title backup-panel-heading backup-subtitle">
          <div><h3>Restore folder</h3><p className="hint">{restoreBackupDir || '/var/backups/opanel-ent/users/restore'}</p></div>
          <div className="actions">
            <button disabled={!!loading} onClick={loadRestoreBackups}><RefreshCw size={14}/> Refresh</button>
            <label className="upload-button">
              <Upload size={14}/> Upload backups
              <input type="file" multiple accept=".tar.gz,application/gzip" onChange={e => { uploadUserBackups(e.target.files); e.target.value = ''; }} />
            </label>
          </div>
        </div>
        <div className="backup-list">
          {restoreBackups.map(item => <div className="backup-item" key={item.backup_file}>
            <span>{item.filename || item.backup_file.split('/').pop()}<small>{item.valid ? `${item.username || 'unknown user'} - ${item.websites || 0} website(s)` : (item.error || 'Invalid backup')}</small></span>
            <div className="actions">
              <button disabled={!!loading} onClick={() => downloadUserBackup(item.backup_file)}><Download size={14}/> Download</button>
              <button disabled={!!loading || !item.valid} onClick={() => restoreUserBackup(item.backup_file)}><RotateCcw size={14}/> Restore user</button>
              <button className="danger" disabled={!!loading} onClick={() => deleteRestoreBackup(item.backup_file)}><Trash2 size={14}/></button>
            </div>
          </div>)}
        </div>

      </div>}

      {isAdmin && activeBackupTab === 'da-import' && <div className="backup-tab-panel">
        <div className="backup-panel-title">
          <div><h3>Import DirectAdmin Backups</h3><p className="hint">Upload and import DirectAdmin user backups ({daBackupDir || '/home/admin/opanel-ent-backups/da'}). Archives are extracted, users/websites/databases created, and OLS vhosts configured automatically.</p></div>
          <div className="actions">
            <button disabled={!!loading} onClick={() => { loadDaBackups(); loadDaImportJobs(); }}><RefreshCw size={14}/> Refresh</button>
            <label className="upload-button">
              <Upload size={14}/> Upload archive
              <input type="file" multiple accept=".tar.gz,.tar.bz2,.tar.xz,.tar.zst,.tar,.tgz,.tbz2,.txz" onChange={e => { uploadDaBackups(e.target.files); e.target.value = ''; }} />
            </label>
          </div>
        </div>

        <h4 style={{margin: '1rem 0 0.5rem'}}>Archives</h4>
        {daBackups.length === 0 && <EmptyState icon={Archive} message="No DA backup archives found. Upload a DirectAdmin backup to get started." />}
        <div className="backup-list">
          {daBackups.map(item => <div className="backup-item" key={item.filename}>
            <span>{item.filename}<small>{(item.size / 1024 / 1024).toFixed(1)} MB</small></span>
            <div className="actions">
              <button disabled={!!loading} onClick={() => startDaImport(item.filename)}><RotateCcw size={14}/> Import</button>
              <button className="danger" disabled={!!loading} onClick={() => deleteDaBackup(item.filename)}><Trash2 size={14}/></button>
            </div>
          </div>)}
        </div>

        {daImportJobs.length > 0 && <>
          <h4 style={{margin: '1.5rem 0 0.5rem'}}>Import Jobs</h4>
          <div className="backup-list">
            {daImportJobs.map(job => <div className="backup-item" key={job.job_id}>
              <span>
                {job.backup_file}
                <span className={job.status === 'done' ? 'badge ok' : job.status === 'error' ? 'badge bad' : 'badge'} style={{marginLeft: '0.5rem'}}>{job.status}</span>
                <small>{job.message || '...'}</small>
                {job.summary && <small style={{whiteSpace: 'pre-wrap'}}>
                  Domains: {job.summary.imported_domains?.join(', ') || 'none'}{job.summary.subdomains?.length ? ` | Subdomains: ${job.summary.subdomains.length}` : ''}{job.summary.databases?.length ? ` | DBs: ${job.summary.databases.length}` : ''}{job.summary.ssl_enabled_domains?.length ? ` | SSL: ${job.summary.ssl_enabled_domains.join(', ')}` : ''}
                </small>}
                {job.error && <small style={{color: 'var(--danger)'}}>{job.error}</small>}
              </span>
            </div>)}
          </div>
        </>}
      </div>}

      {isAdmin && activeBackupTab === 'schedule' && <div className="backup-tab-panel">
        <div className="backup-panel-title">
          <div><h3>Scheduled backups</h3><p className="hint">Run full user backups automatically with optional off-server destination.</p></div>
          <button disabled={!!loading} onClick={refreshScheduledBackupArea}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="sftp-form schedule-form backup-schedule-form">
          <label className="schedule-toggle">
            <input type="checkbox" checked={!!newBackupSchedule.all_users} onChange={e => setNewBackupSchedule(prev => ({ ...prev, all_users: e.target.checked }))} />
            <span>All users</span>
          </label>
          <select multiple value={newBackupSchedule.user_ids || []} disabled={!!newBackupSchedule.all_users} onChange={e => setNewBackupSchedule(prev => ({ ...prev, user_ids: Array.from(e.target.selectedOptions, option => option.value) }))}>
            {users.map(user => <option key={user.id} value={String(user.id)}>{user.username}</option>)}
          </select>
          <input value={newBackupSchedule.schedule} onChange={e => setNewBackupSchedule(prev => ({ ...prev, schedule: e.target.value }))} placeholder="0 2 * * *" />
          <select value={newBackupSchedule.target_id} onChange={e => setNewBackupSchedule(prev => ({ ...prev, target_id: e.target.value }))}>
            <option value="">Local only</option>
            {sftpTargets.map(target => <option key={target.id} value={target.id}>{target.name}</option>)}
          </select>
          <button disabled={(!newBackupSchedule.all_users && (!newBackupSchedule.user_ids || newBackupSchedule.user_ids.length === 0)) || !!loading} onClick={createBackupSchedule}><Clock size={14}/> Schedule</button>
        </div>
        <div className="backup-list">
          {backupSchedules.map(item => {
            const scheduleTarget = sftpTargets.find(target => target.id === item.target_id);
            return <div className="backup-item" key={item.id}>
              <span>{scheduleUserLabel(item)} - {item.schedule}{scheduleTarget ? ` - ${scheduleTarget.name}` : ''}<small>{item.last_status}: {item.last_message || 'not run yet'}</small></span>
              <button className="danger" disabled={!!loading} onClick={() => deleteBackupSchedule(item.id)}><Trash2 size={14}/></button>
            </div>;
          })}
        </div>
      </div>}

      {isAdmin && activeBackupTab === 'destination' && <div className="backup-tab-panel">
        <div className="backup-panel-title">
          <div><h3>Backup Destination</h3><p className="hint">Manage SFTP destinations used for off-server backup copies.</p></div>
          <button disabled={!!loading} onClick={loadSftpTargets}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="sftp-form sftp-target-form">
          <input value={newSftpTarget.name} onChange={e => setNewSftpTarget(prev => ({ ...prev, name: e.target.value }))} placeholder="Target name" />
          <input value={newSftpTarget.host} onChange={e => setNewSftpTarget(prev => ({ ...prev, host: e.target.value }))} placeholder="Host" />
          <input value={newSftpTarget.port} onChange={e => setNewSftpTarget(prev => ({ ...prev, port: e.target.value }))} placeholder="22" inputMode="numeric" />
          <input value={newSftpTarget.username} onChange={e => setNewSftpTarget(prev => ({ ...prev, username: e.target.value }))} placeholder="Username" />
          <input value={newSftpTarget.password} onChange={e => setNewSftpTarget(prev => ({ ...prev, password: e.target.value }))} placeholder="Password" type="password" />
          <input value={newSftpTarget.remote_path} onChange={e => setNewSftpTarget(prev => ({ ...prev, remote_path: e.target.value }))} placeholder="/backups/opanel-ent" />
          <textarea value={newSftpTarget.private_key} onChange={e => setNewSftpTarget(prev => ({ ...prev, private_key: e.target.value }))} placeholder="Private key (optional)" rows={4} />
          <button disabled={!!loading || !newSftpTarget.name || !newSftpTarget.host || !newSftpTarget.username || (!newSftpTarget.password && !newSftpTarget.private_key)} onClick={createSftpTarget}><Plus size={14}/> Save target</button>
        </div>
        {sftpTargets.length === 0 && <EmptyState icon={Network} message="No backup destinations found." />}
        <div className="backup-list">
          {sftpTargets.map(target => <div className="backup-item" key={target.id}>
            <span>{target.name} - {target.username}@{target.host}:{target.remote_path}</span>
            <button className="danger" disabled={!!loading} onClick={() => deleteSftpTarget(target.id)}><Trash2 size={14}/></button>
          </div>)}
        </div>
      </div>}
    </section>;
  }

  function renderServices() {
    return <section className="section">
      <div className="section-title">
        <h2>Services Status</h2>
        <button disabled={!!loading} onClick={checkAllServices}><RefreshCw size={15}/> Refresh</button>
      </div>
      <div className="service-grid">
        {serviceNames.map(name => {
          const state = serviceStates[name];
          const text = `${state?.stdout || ''} ${state?.stderr || ''}`;
          const active = text.includes('active (running)');
          const inactive = text.includes('inactive') || text.includes('failed');
          return <div className="service-card" key={name}>
            <div><strong>{name}</strong><span className={active ? 'badge ok' : inactive ? 'badge bad' : 'badge'}>{active ? 'Running' : inactive ? 'Stopped' : '...'}</span></div>
            <small>Auto-refreshes every 10s</small>
            {isAdmin && <div className="service-actions">
              <button onClick={() => runServiceAction(name, 'start')}><Play size={13}/> Start</button>
              {!['opanel-ent-api', 'redis-server'].includes(name) && <button onClick={() => runServiceAction(name, 'stop')}><Square size={13}/> Stop</button>}
              <button onClick={() => runServiceAction(name, 'restart')}><RotateCcw size={13}/> Restart</button>
            </div>}
          </div>;
        })}
      </div>
    </section>;
  }

  function renderPhpConfig() {
    if (!isAdmin) return <section className="section"><h2>PHP config</h2><p className="hint">You do not have permission to edit PHP config.</p></section>;
    const notInstalled = sortPhpVersions(phpVersions.supported.filter(v => !phpVersions.installed.includes(v)));
    return <section className="section">
      <div className="section-title">
        <div><h2>PHP Configuration</h2></div>
      </div>
      <div className="user-create-card">
        <label><span>PHP version</span><select value={phpConfig.php_version} onChange={e => { const v = e.target.value; setPhpConfig(prev => ({ ...prev, php_version: v })); loadPhpConfig(v); }}>
          {phpVersionOptions(phpVersions.installed, phpConfig.php_version).map(v => <option key={v} value={v}>PHP {v}</option>)}
        </select></label>
        <label><span>display_errors</span><select value={phpConfig.display_errors} onChange={e => setPhpConfig(prev => ({ ...prev, display_errors: e.target.value }))}>
          <option value="Off">Off (production)</option><option value="On">On (debug)</option>
        </select></label>
        <label className="php-opcache-toggle"><span>OPcache</span>
          <button
            type="button"
            className={phpConfig.opcache_enable ? '' : 'secondary'}
            aria-pressed={!!phpConfig.opcache_enable}
            disabled={!!loading}
            onClick={() => setPhpConfig(prev => ({ ...prev, opcache_enable: !prev.opcache_enable }))}
            title={`OPcache is ${phpConfig.opcache_enable ? 'on' : 'off'} for PHP ${phpConfig.php_version}. Press Save to apply.`}
          >
            <Zap size={14}/> {phpConfig.opcache_enable ? 'Enabled' : 'Disabled'}
          </button>
        </label>
        <label><span>max_execution_time</span><input type="number" value={phpConfig.max_execution_time} onChange={e => setPhpConfig(prev => ({ ...prev, max_execution_time: e.target.value }))} /></label>
        <label><span>max_input_time</span><input type="number" value={phpConfig.max_input_time} onChange={e => setPhpConfig(prev => ({ ...prev, max_input_time: e.target.value }))} /></label>
        <label><span>max_input_vars</span><input type="number" value={phpConfig.max_input_vars} onChange={e => setPhpConfig(prev => ({ ...prev, max_input_vars: e.target.value }))} /></label>
        <label><span>memory_limit</span><input value={phpConfig.memory_limit} onChange={e => setPhpConfig(prev => ({ ...prev, memory_limit: e.target.value }))} placeholder="1024M" /></label>
        <label><span>post_max_size</span><input value={phpConfig.post_max_size} onChange={e => setPhpConfig(prev => ({ ...prev, post_max_size: e.target.value }))} placeholder="1024M" /></label>
        <label><span>upload_max_filesize</span><input value={phpConfig.upload_max_filesize} onChange={e => setPhpConfig(prev => ({ ...prev, upload_max_filesize: e.target.value }))} placeholder="1024M" /></label>
        <button className="secondary-light" disabled={!!loading} onClick={restorePhpDefaults}><RotateCcw size={14}/> Restore defaults</button>
        <button className="secondary-light" disabled={!!loading} onClick={loadPhpTuning}><Zap size={14}/> Auto-tune</button>
        <button disabled={!!loading} onClick={updatePhpConfig}>Save</button>
      </div>
      {phpTuning && phpTuning.recommendation && <div className="user-create-card" style={{ marginTop: 16, borderColor: 'var(--accent)' }}>
        <h3>⚡ Auto-tune Recommendation</h3>
        <p className="hint">Based on {phpTuning.recommendation.ram_mb} MB RAM, {phpTuning.recommendation.cores} CPU cores, {phpTuning.recommendation.is_ssd ? 'SSD' : 'HDD'} storage</p>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '8px 24px', margin: '12px 0', fontSize: '0.9em' }}>
          <div><strong>memory_limit:</strong> {phpTuning.recommendation.memory_limit}</div>
          <div><strong>OPcache memory:</strong> {phpTuning.recommendation.opcache_memory_consumption} MB</div>
          <div><strong>OPcache files:</strong> {phpTuning.recommendation.opcache_max_accelerated_files}</div>
          <div><strong>OPcache JIT:</strong> {phpTuning.recommendation.opcache_jit} ({phpTuning.recommendation.opcache_jit_buffer_size}MB)</div>
          <div><strong>LSAPI workers:</strong> {phpTuning.recommendation.lsapi_children}</div>
          <div><strong>LSAPI idle:</strong> {phpTuning.recommendation.lsapi_max_idle}s, max idle children: {phpTuning.recommendation.lsapi_max_idle_children}</div>
          <div><strong>upload_max:</strong> {phpTuning.recommendation.upload_max_filesize}</div>
          <div><strong>post_max:</strong> {phpTuning.recommendation.post_max_size}</div>
        </div>
        <button disabled={!!loading} onClick={applyPhpTuning}><Zap size={14}/> Apply Auto-tune to PHP {phpConfig.php_version}</button>
        <button className="secondary-light" style={{ marginLeft: 8 }} onClick={() => setPhpTuning(null)}>Dismiss</button>
      </div>}
      {notInstalled.length > 0 && <div className="user-create-card" style={{ marginTop: 16 }}>
        <h3>Install PHP</h3>
        <div className="php-install-grid">
          {notInstalled.map(v => <button key={v} disabled={!!loading} onClick={() => installPhpVersion(v)}>+ PHP {v}</button>)}
        </div>
      </div>}
    </section>;
  }

  function renderFirewall() {
    if (!isAdmin) return <section className="section"><h2>Firewall</h2><p className="hint">No permission.</p></section>;
    const firewallText = firewallStatus?.stdout || firewallStatus?.stderr || 'Click Refresh to load status.';
    const blocklistText = firewallBlocklists?.stdout || firewallBlocklists?.stderr || 'No blocklist status loaded.';
    const blocklistUrls = parseFirewallBlocklistUrls(blocklistText);
    const openPorts = Array.isArray(firewallStatus?.open_ports) ? firewallStatus.open_ports : [];
    return <>
      <section className="section">
        <div className="section-title">
          <div><h2>Firewall (iptables)</h2><p className="hint">Keep SSH and web ports allowed before enabling.</p></div>
        </div>
        <div className="actions">
          <button disabled={!!loading} onClick={loadFirewall}><RefreshCw size={14}/> Refresh</button>
          <button disabled={!!loading} onClick={enableFirewall}><Shield size={14}/> Enable</button>
          <button disabled={!!loading} onClick={disableFirewall}>Disable</button>
          <button disabled={!!loading} onClick={reloadFirewall}>Reload</button>
        </div>
        <div className="info-box firewall-open-ports">
          <strong>Open ports</strong>
          {openPorts.length > 0 ? <div className="firewall-port-list">
            {openPorts.map(item => <span className="firewall-port-chip" key={`${item.protocol}-${item.port}-${item.zone}-${item.rule || ''}`}>
              <code>{item.port}/{String(item.protocol || 'tcp').toUpperCase()}</code>
              <small>{item.zone || 'UserZone'}{item.source && item.source !== 'Anywhere' ? ` from ${item.source}` : ''}</small>
            </span>)}
          </div> : <p className="hint">No open port rules found in OPANEL_ENT chains.</p>}
        </div>
        <div className="info-box firewall-status">
          <strong>iptables status</strong>
          <pre>{firewallText}</pre>
          <div className="firewall-delete-inline">
            <label><span>Delete UserZone #</span><input value={firewallDeleteNumber} onChange={e => setFirewallDeleteNumber(e.target.value)} placeholder="12" inputMode="numeric" /></label>
            <button className="danger" disabled={!!loading || !firewallDeleteNumber} onClick={() => deleteFirewallRule()}>Delete</button>
          </div>
        </div>
      </section>
      <section className="section">
        <h2>Open port</h2>
        <div className="firewall-form">
          <label><span>Port</span><input value={firewallPort} onChange={e => setFirewallPort(e.target.value)} placeholder="80" inputMode="numeric" /></label>
          <label><span>Protocol</span><select value={firewallProtocol} onChange={e => setFirewallProtocol(e.target.value)}><option value="tcp">TCP</option><option value="udp">UDP</option></select></label>
          <button disabled={!!loading || !firewallPort} onClick={openFirewallPort}>Open port</button>
        </div>
      </section>
      <section className="section">
        <h2>Allow IP</h2>
        <div className="firewall-form">
          <label><span>IP / CIDR</span><input value={firewallAllowIp} onChange={e => setFirewallAllowIp(e.target.value)} placeholder="1.2.3.4" /></label>
          <label><span>Port (optional)</span><input value={firewallAllowPort} onChange={e => setFirewallAllowPort(e.target.value)} placeholder="22" inputMode="numeric" /></label>
          <label><span>Protocol</span><select value={firewallAllowProtocol} onChange={e => setFirewallAllowProtocol(e.target.value)}><option value="tcp">TCP</option><option value="udp">UDP</option></select></label>
          <button disabled={!!loading || !firewallAllowIp} onClick={allowFirewallIp}>Allow</button>
        </div>
      </section>
      <section className="section">
        <h2>Block IP</h2>
        <div className="firewall-form">
          <label><span>IP / CIDR</span><input value={firewallBlockIp} onChange={e => setFirewallBlockIp(e.target.value)} placeholder="5.6.7.8" /></label>
          <label><span>Port (optional)</span><input value={firewallBlockPort} onChange={e => setFirewallBlockPort(e.target.value)} placeholder="All ports" inputMode="numeric" /></label>
          <label><span>Protocol</span><select value={firewallBlockProtocol} onChange={e => setFirewallBlockProtocol(e.target.value)}><option value="tcp">TCP</option><option value="udp">UDP</option></select></label>
          <button className="danger" disabled={!!loading || !firewallBlockIp} onClick={blockFirewallIp}>Block</button>
        </div>
      </section>
      <section className="section">
        <div className="section-title">
          <div><h2>Blocklist URLs</h2><p className="hint">TXT files are fetched daily at 01:00 and enforced by ipset, so large lists do not create thousands of firewall rules.</p></div>
          <button disabled={!!loading} onClick={loadFirewallBlocklists}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="firewall-form firewall-blocklist-form">
          <label><span>TXT URL</span><input value={firewallBlocklistUrl} onChange={e => setFirewallBlocklistUrl(e.target.value)} placeholder="https://example.com/blocklist.txt" /></label>
          <button disabled={!!loading || !firewallBlocklistUrl.trim()} onClick={addFirewallBlocklistUrl}><Plus size={14}/> Add URL</button>
          <button className="secondary-light" disabled={!!loading} onClick={updateFirewallBlocklistsNow}><RefreshCw size={14}/> Update now</button>
        </div>
        {blocklistUrls.length > 0 && <div className="table firewall-blocklist-table">
          {blocklistUrls.map(url => <div className="firewall-rule" key={url}>
            <span>{url}</span>
            <div className="firewall-rule-actions"><button className="danger" disabled={!!loading} onClick={() => deleteFirewallBlocklistUrl(url)}><Trash2 size={14}/> Delete</button></div>
          </div>)}
        </div>}
        <div className="info-box firewall-status"><strong>Blocklist status</strong><pre>{blocklistText}</pre></div>
      </section>
    </>;
  }

  function renderWaf() {
    const statusText = wafRules.status?.stdout || wafRules.status?.stderr || 'Click Refresh to load WAF status.';
    const selectedSite = websites.find(site => String(site.id) === String(selectedWafWebsiteId));
    const groupedRules = (wafSiteConfig?.default_rules || wafRules.default_rule_definitions || []).reduce((groups, rule) => {
      const category = rule.category || 'General';
      groups[category] = groups[category] || [];
      groups[category].push(rule);
      return groups;
    }, {});
    return <>
      {!wafSiteConfig && <>
        {isAdmin && <section className="section">
          <div className="section-title">
            <div><h2>WAF</h2><p className="hint">Engine status. Rules are configured per website below.</p></div>
            <button disabled={!!loading} onClick={loadWafRules}><RefreshCw size={14}/> Refresh</button>
          </div>
          <div className="info-box firewall-status"><strong>Status</strong><pre>{statusText}</pre></div>
        </section>}

        {isAdmin && <section className="section">
          <div className="section-title">
            <div>
              <h2>Global bad bot <span className="badge">{badBots.patterns.length}</span></h2>
              <p className="hint">One bot per line, applied to every website. A request whose User-Agent contains the text gets 403 — plain text, no regex.</p>
            </div>
            <button className="secondary" disabled={!!loading} onClick={loadBadBots}><RefreshCw size={14}/> Refresh</button>
          </div>
          <textarea className="code-editor badbot-input" value={badBotText} onChange={e => setBadBotText(e.target.value)}
            spellCheck={false}
            placeholder={"AhrefsBot\nSemrushBot\nMJ12bot\nGPTBot\nBytespider"} />
          <div className="actions"><button disabled={!!loading} onClick={saveBadBots}><Shield size={14}/> Save and apply to all websites</button></div>
        </section>}

        <section className="section">
          <div className="section-title">
            <div><h2>{isAdmin ? 'Websites' : 'Your websites'}</h2><p className="hint">Open a website to configure its rules and bad bots.</p></div>
          </div>
          {websites.length === 0
            ? <EmptyState icon={Globe} message="No websites yet." />
            : <div className="waf-site-table">
                {websites.map(site => <button key={site.id} type="button" className="waf-site-row"
                  disabled={!!loading} onClick={() => loadWebsiteWafConfig(site.id)}>
                  <span className="waf-site-domain">{site.domain}</span>
                  <span className="waf-site-badges">
                    <span className={site.waf_enabled ? 'badge ok' : 'badge'}>{site.waf_enabled ? 'WAF on' : 'WAF off'}</span>
                  </span>
                  <span className="waf-site-open">Configure</span>
                </button>)}
              </div>}
        </section>
      </>}

      {wafSiteConfig && <>
        <section className="section">
          <div className="section-title waf-detail-head">
            <div className="waf-detail-title">
              <button className="secondary-light" onClick={closeWafSiteConfig}><ArrowLeft size={14}/> All websites</button>
              <div><h2>{wafSiteConfig.domain}</h2><p className="hint">WAF configuration for this website.</p></div>
            </div>
            <div className="waf-detail-actions">
              <span className={selectedSite?.waf_enabled ? 'badge ok' : 'badge'}>{selectedSite?.waf_enabled ? 'WAF on' : 'WAF off'}</span>
              <button disabled={!!loading} onClick={() => selectedSite && toggleWebsiteWaf(selectedSite)}>
                <Shield size={14}/> {selectedSite?.waf_enabled ? 'Disable WAF' : 'Enable WAF'}
              </button>
            </div>
          </div>
        </section>

        <section className="section">
          <div className="section-title">
            <div>
              <h2>Bad bot</h2>
              <p className="hint">
                {wafSiteConfig.bot_blocking_enabled
                  ? `Global list (${(wafSiteConfig.global_bad_bots || []).length}) plus anything below.`
                  : 'Off — the global list is ignored on this website.'}
              </p>
            </div>
            <span className={wafSiteConfig.bot_blocking_enabled ? 'badge ok' : 'badge'}>
              {wafSiteConfig.bot_blocking_enabled ? 'On' : 'Off'}
            </span>
          </div>
          <label className="check-line">
            <input type="checkbox" checked={!!wafSiteConfig.bot_blocking_enabled}
              onChange={e => setWafSiteConfig(prev => ({ ...prev, bot_blocking_enabled: e.target.checked }))} />
            Block bad bots on this website
          </label>
          <label className="badbot-site-extra"><span>Extra bots, this website only</span>
            <textarea className="code-editor badbot-input" value={wafBotExtra} onChange={e => setWafBotExtra(e.target.value)}
              spellCheck={false} placeholder={"ScrapyBot\nSomeOtherBot"} />
          </label>
          <div className="actions"><button disabled={!!loading} onClick={saveWebsiteWafRules}><Shield size={14}/> Save bad bot config</button></div>
        </section>

        <section className="section waf-rules-grid">
          <div className="waf-rule-panel">
            <div className="section-title"><h2>Default rules</h2></div>
            <div className="waf-default-groups">
              {Object.entries(groupedRules).map(([category, rules]) => <div className="waf-rule-group" key={category}>
                <h3>{category}</h3>
                {rules.map(rule => <label className="waf-rule-toggle" key={rule.id}>
                  <input type="checkbox" checked={!!rule.enabled} onChange={e => toggleWafDefaultRule(rule.id, e.target.checked)} />
                  <span><strong>{rule.title}</strong><small>{rule.description}</small></span>
                </label>)}
              </div>)}
            </div>
          </div>
          <div className="waf-rule-panel">
            <div className="section-title"><h2>Custom rules</h2></div>
            {isAdmin
              ? <>
                  <textarea className="code-editor" value={wafCustomRules} onChange={e => setWafCustomRules(e.target.value)} rows={14} spellCheck={false} placeholder="SecRule ..." />
                  <p className="hint">Saved into {wafSiteConfig.rules_file}</p>
                </>
              : <>
                  {wafCustomRules
                    ? <pre className="code-editor waf-custom-readonly">{wafCustomRules}</pre>
                    : <p className="hint">No custom rules on this website.</p>}
                  <p className="hint">Custom rules are written by an administrator. Everything else on this page is yours to change.</p>
                </>}
            <div className="actions"><button disabled={!!loading} onClick={saveWebsiteWafRules}>Save website WAF rules</button></div>
          </div>
        </section>
      </>}
    </>;
  }

  function renderWafAccessLogs() {
    // The API scopes every report to the caller's own domains, so an end user
    // sees what was blocked on their sites and nothing from anyone else's.
    const entries = wafAccessLogs.entries || [];
    const total = Number(wafAccessLogs.total || 0);
    const limit = Number(wafAccessFilters.limit || wafAccessLogs.limit || 50);
    const offset = Number(wafAccessFilters.offset || wafAccessLogs.offset || 0);
    const currentPage = Math.floor(offset / limit) + 1;
    const pageCount = Math.max(1, Math.ceil(total / limit));
    const domains = wafAccessLogs.domains?.length ? wafAccessLogs.domains : websites.map(site => site.domain);
    return <>
      <section className="access-log-hero">
        <div>
          <p className="eyebrow">Protected Traffic</p>
          <h1>Access Logs</h1>
        </div>
        <div className="access-log-hero-actions">
          <button className="secondary-light icon-only" onClick={() => loadWafAccessLogs()} disabled={!!loading} aria-label="Refresh logs" title="Refresh logs"><RefreshCw size={16}/></button>
          <button className="secondary-light icon-only" onClick={() => navigateToPage('waf')} aria-label="Open WAF settings" title="Open WAF settings"><ExternalLink size={16}/></button>
        </div>
      </section>
      <section className="section access-log-section">
        <div className="access-log-toolbar">
          <div className="access-log-title">
            <strong>Access Logs</strong>
            <span>{formatLogCount(total)} entries</span>
            <button className="secondary-light" onClick={exportWafAccessLogs} disabled={!entries.length}><Download size={14}/> Export</button>
            <button className="danger-light" onClick={clearWafAccessLogs} disabled={!!loading || total === 0}><Trash2 size={14}/> Clear</button>
          </div>
          <div className="access-log-filters">
            <select value={wafAccessFilters.domain} onChange={e => setWafAccessFilter('domain', e.target.value)}>
              <option value="">All websites</option>
              {domains.map(domain => <option key={domain} value={domain}>{domain}</option>)}
            </select>
            <select value={wafAccessFilters.verdict} onChange={e => setWafAccessFilter('verdict', e.target.value)}>
              <option value="">All verdicts</option>
              <option value="block">Block</option>
              <option value="allow">Allow</option>
            </select>
            <input value={wafAccessFilters.q} onChange={e => setWafAccessFilter('q', e.target.value)} placeholder="Filter logs" />
            <select value={limit} onChange={e => setWafAccessFilter('limit', Number(e.target.value))}>
              {[25, 50, 100, 250, 500].map(size => <option key={size} value={size}>{size} / page</option>)}
            </select>
            <select value={wafAccessAutoRefresh} onChange={e => setWafAccessAutoRefresh(Number(e.target.value))} title="Auto refresh">
              <option value={0}>Auto refresh off</option>
              <option value={5}>Refresh 5s</option>
              <option value={10}>Refresh 10s</option>
              <option value={30}>Refresh 30s</option>
            </select>
          </div>
        </div>
        <div className="access-log-table-wrap">
          <table className="access-log-table">
            <thead>
              <tr>
                <th>Verdict</th>
                <th>Time</th>
                <th>Site</th>
                <th>Method</th>
                <th>Path</th>
                <th>IP</th>
                <th>Reason</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((entry, index) => <tr key={`${entry.domain}-${entry.timestamp}-${entry.ip}-${entry.path}-${index}`}>
                <td><span className={`verdict-pill ${entry.verdict === 'block' ? 'block' : 'allow'}`}>{entry.verdict === 'block' ? 'Block' : 'Allow'}</span></td>
                <td><span>{entry.time || entry.timestamp || '-'}</span><small>{formatLogDuration(entry.duration_ms)}</small></td>
                <td><span>{entry.site || entry.domain}</span><small>{entry.domain}</small></td>
                <td>{entry.method || '-'}</td>
                <td><span className="access-log-path">{entry.path || '-'}</span></td>
                <td><span>{entry.ip || '-'}</span><small>{entry.country || ''}</small></td>
                <td>{entry.reason || (entry.verdict === 'block' ? 'Blocked' : 'Allowed')}</td>
                <td>{entry.status || '-'}</td>
              </tr>)}
            </tbody>
          </table>
          {entries.length === 0 && <EmptyState icon={Shield} message="No access log entries match the current filters." />}
        </div>
        <div className="access-log-footer">
          <span>Page {currentPage} of {pageCount}</span>
          <div className="actions">
            <button className="secondary-light" disabled={offset <= 0 || !!loading} onClick={() => loadWafAccessLogs({ ...wafAccessFilters, offset: Math.max(0, offset - limit) })}>Previous</button>
            <button className="secondary-light" disabled={offset + limit >= total || !!loading} onClick={() => loadWafAccessLogs({ ...wafAccessFilters, offset: offset + limit })}>Next</button>
          </div>
        </div>
      </section>
    </>;
  }

  function renderUpdates() {
    if (!isAdmin) return <section className="section"><h2>Updates</h2><p className="hint">No permission.</p></section>;
    const statusText = updatesStatus?.stdout || updatesStatus?.stderr || 'Click View logs to load update logs.';
    const panelUpdate = updatesStatus?.panel || {};
    const panelBadge = panelUpdate.latest_commit ? 'Tracking main' : 'Unknown';
    const panelBadgeClass = panelUpdate.latest_commit ? 'badge ok' : 'badge';
    const currentPanelVersion = panelUpdate.current_version || appVersion || 'unknown';
    const latestPanelRef = panelUpdate.latest_commit ? panelUpdate.latest_commit.slice(0, 12) : 'unknown';
    return <>
      <section className="section">
        <div className="section-title">
          <div><h2>Updates</h2><p className="hint">OS packages use apt; panel updates pull from GitHub <code>main</code>.</p></div>
          <button className="secondary-light" disabled={!!loading} onClick={toggleUpdateLog}>{showUpdateLog ? <X size={14}/> : <FileText size={14}/>} {showUpdateLog ? 'Hide logs' : 'View logs'}</button>
        </div>
        <div className="info-box update-version-box">
          <div className="update-version-head"><strong>Panel source</strong><span className={panelBadgeClass}>{panelBadge}</span></div>
          <div className="update-version-grid">
            <span>Current <strong>v{currentPanelVersion}</strong></span>
            <span>Branch <strong>{panelUpdate.update_branch || 'main'}</strong></span>
            <span>Latest commit <strong>{latestPanelRef}</strong></span>
            <span>Checked <strong>{panelUpdate.last_checked_at || 'never'}</strong></span>
            <span>State file <strong>{panelUpdate.state_file || '/var/lib/opanel-ent/update-status.json'}</strong></span>
          </div>
          {panelUpdate.check_error && <p className="hint">Main branch check failed: {panelUpdate.check_error}</p>}
          {panelUpdate.last_update_status && <p className="hint">Last update: {panelUpdate.last_update_status}{panelUpdate.last_update_ref ? ` (${panelUpdate.last_update_ref})` : ''}{panelUpdate.last_update_finished_at ? ` at ${panelUpdate.last_update_finished_at}` : ''}</p>}
        </div>
        <div className="actions">
          <button className="secondary-light" disabled={!!loading} onClick={() => loadUpdates(true)}><RefreshCw size={14}/> Refresh status</button>
          <button disabled={!!loading || osUpdating} onClick={runOsUpdate}><RefreshCw size={14} className={osUpdating ? 'spin' : ''}/> {osUpdating ? 'Updating OS...' : 'Update OS now'}</button>
          <button disabled={!!loading || panelUpdating} onClick={runPanelUpdate}><RotateCcw size={14} className={panelUpdating ? 'spin' : ''}/> {panelUpdating ? 'Updating panel...' : 'Update panel now'}</button>
        </div>
        {showUpdateLog && <div className="info-box firewall-status update-log-box">
          <div className="update-log-head"><strong>Update logs</strong><button className="secondary-light" disabled={!!loading} onClick={() => loadUpdates(true)}><RefreshCw size={13}/> Refresh</button></div>
          <pre>{statusText}</pre>
        </div>}
        {panelUpdate.last_update_status !== 'completed' && panelUpdate.last_update_status !== 'failed' && (panelUpdating || (Boolean(panelUpdate.progress_percent) && panelUpdate.last_update_status)) && (
          <div className="info-box firewall-status update-progress-box">
            <div className="update-progress-row">
              <span className={panelUpdate.last_update_status === 'failed' ? 'badge bad' : 'badge ok'}>
                {panelUpdating ? 'Running' : (panelUpdate.last_update_status === 'failed' ? 'Failed' : (panelUpdate.last_update_status || 'Idle'))}
              </span>
              <span className="update-progress-phase">{panelUpdate.progress_phase || ''}</span>
              <span className="update-progress-pct">{Number(panelUpdate.progress_percent) || 0}%</span>
            </div>
            <div className="progress-bar"><div className="progress-bar-fill" style={{ width: `${Number(panelUpdate.progress_percent) || 0}%` }} /></div>
            {panelUpdate.progress_message && <p className="hint update-progress-msg">{panelUpdate.progress_message}</p>}
            {panelUpdateLog.length > 0 && (
              <pre className="update-progress-log">{panelUpdateLog.join('\n')}</pre>
            )}
          </div>
        )}
      </section>
      <section className="section">
        <h2>Auto Update OS</h2>
        <div className="firewall-form updates-os-form">
          <label><span>Enabled</span><select value={osAutoUpdate.enabled ? 'on' : 'off'} onChange={e => setOsAutoUpdate(prev => ({ ...prev, enabled: e.target.value === 'on' }))}><option value="on">On</option><option value="off">Off</option></select></label>
          <label><span>Mode</span><select value={osAutoUpdate.mode} onChange={e => setOsAutoUpdate(prev => ({ ...prev, mode: e.target.value }))}><option value="security">Security</option><option value="all">All packages</option></select></label>
          <label><span>Auto reboot</span><select value={osAutoUpdate.auto_reboot ? 'on' : 'off'} onChange={e => setOsAutoUpdate(prev => ({ ...prev, auto_reboot: e.target.value === 'on' }))}><option value="off">Off</option><option value="on">On</option></select></label>
          <button disabled={!!loading} onClick={saveOsAutoUpdate}>Save OS auto update</button>
        </div>
      </section>
    </>;
  }

  function renderSecurity() {
    const enabled = Boolean(twoFactorStatus?.enabled || currentUser?.totp_enabled);
    return <>
      <section className="section">
        <div className="section-title">
          <div><h2>Google Authenticator 2FA</h2><p className="hint">Current status: <strong>{enabled ? 'Enabled' : 'Disabled'}</strong></p></div>
          <button disabled={!!loading} onClick={loadTwoFactorStatus}><RefreshCw size={14}/> Refresh</button>
        </div>
        {!enabled && <div className="security-grid">
          <div className="info-box">
            <strong>Setup</strong>
            {twoFactorSetup?.qr_data_url ? <img className="qr-code" src={twoFactorSetup.qr_data_url} alt="2FA QR code" /> : <p className="hint">No setup code generated.</p>}
            {twoFactorSetup?.secret && <code className="secret-text">{twoFactorSetup.secret}</code>}
            <div className="actions">
              <button disabled={!!loading} onClick={setupTwoFactorAuth}><Shield size={14}/> Generate QR</button>
            </div>
          </div>
          <div className="info-box">
            <strong>Verify</strong>
            <input value={twoFactorCode} onChange={e => setTwoFactorCode(e.target.value)} placeholder="123456" inputMode="numeric" />
            <button disabled={!!loading || !twoFactorSetup || !twoFactorCode} onClick={enableTwoFactorAuth}><Lock size={14}/> Enable 2FA</button>
          </div>
        </div>}
        {enabled && <div className="security-grid one">
          <div className="info-box">
            <strong>Disable 2FA</strong>
            <input value={twoFactorCode} onChange={e => setTwoFactorCode(e.target.value)} placeholder="123456" inputMode="numeric" />
            <button className="danger" disabled={!!loading || !twoFactorCode} onClick={disableTwoFactorAuth}>Disable 2FA</button>
          </div>
        </div>}
      </section>
    </>;
  }

  function renderMalwareScanner() {
    if (!isAdmin) return <section className="section"><h2>Malware Scanner</h2><p className="hint">No permission.</p></section>;
    const mw = malwareScanStatus || {};
    const mwActive = Boolean(mw.active);
    const mwInstalled = Boolean(mw.installed);
    const mwEnabled = Boolean(mw.enabled);
    const activeScanJob = scanJob || scanResults || {};
    const scanRunning = ['queued', 'running'].includes(scanJob?.status);
    const scanJobTitle = job => (job.domains && job.domains.length > 0)
      ? (job.domains.length === 1 ? job.domains[0] : `${job.domains.length} websites`)
      : (job.scope === 'all' ? 'All websites'
        : job.scope === 'system' ? `Full server (${job.scan_root || '/'})`
        : 'Scan job');
    const scanJobStamp = job => {
      const stamp = job.finished_at || job.updated_at || job.started_at || job.created_at || '';
      if (!stamp) return 'No timestamp';
      const date = new Date(stamp);
      return Number.isNaN(date.getTime()) ? stamp : new Intl.DateTimeFormat('en-GB', {
        timeZone: 'Asia/Ho_Chi_Minh',
        hour12: false,
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        day: '2-digit',
        month: '2-digit',
        year: 'numeric',
      }).format(date).replace(',', '');
    };
    const scanJobDetail = job => `${job.scanned || 0}/${job.total_files || job.scanned || 0} files, ${job.infected || 0} threats, ${job.errors || 0} errors`;
    const scanJobMeta = job => `${scanJobStamp(job)} / ${scanJobDetail(job)}`;
    const scanJobBadgeClass = job => {
      if (job.status === 'done') return 'badge ok';
      if (job.status === 'infected') return 'badge danger';
      if (['error', 'interrupted'].includes(job.status)) return 'badge bad';
      return 'badge warn';
    };
    return <>
      {isAdmin && <section className="section">
        <div className="section-title">
          <div>
            <h2>Malware Scanner</h2>
            <p className="hint">
              {mwActive ? <span className="badge ok">Active</span>
                : mwEnabled && mwInstalled ? <span className="badge warn">Enabled — clamd not running</span>
                : mwEnabled && !mwInstalled ? <span className="badge warn">Installing ClamAV...</span>
                : mwInstalled && !mwEnabled ? <span className="badge">Installed — scanning disabled</span>
                : <span className="badge">Not installed</span>}
              {mwInstalled && <span className="badge" style={{marginLeft:6}}>Engine: {mw.engine || 'ClamAV'}{mw.lmd_version ? ` (LMD ${mw.lmd_version})` : ''}</span>}
              {mw.realtime_active && <span className="badge ok" style={{marginLeft:6}}>Real-time on</span>}
            </p>
          </div>
          <button disabled={!!loading} onClick={loadMalwareScanStatus}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="info-box">
          <p className="hint">{mw.detail || 'Checking status...'}</p>
          {!mwInstalled && <p className="hint" style={{marginTop:8}}>When enabled, ClamAV is installed on this server, and Linux Malware Detect is layered on top of it (its web-focused signatures catch the PHP shells ClamAV misses). Uploaded files are scanned in real-time.</p>}
          {mwInstalled && mwActive && <p className="hint" style={{marginTop:8}}>Uploaded files are scanned automatically. Scheduled scans default to incremental (files changed recently) once a full baseline scan has run, with a full scan at least weekly.</p>}
          <div className="actions" style={{marginTop:12}}>
            {!mwEnabled
              ? <button disabled={!!loading} onClick={() => toggleMalwareScan(true)}><Shield size={14}/> Enable Malware Scanner</button>
              : <button className="danger" disabled={!!loading} onClick={() => toggleMalwareScan(false)}>Disable Malware Scanner</button>
            }
            {mwActive && <button className="secondary" disabled={!!loading} onClick={updateMalwareSignatures}><RefreshCw size={14}/> Update signatures</button>}
          </div>
        </div>

        {mwActive && !mw.lmd_installed && <div className="info-box">
          <div className="malware-scan-head">
            <div>
              <strong>Linux Malware Detect <span className="badge">Not installed</span></strong>
              <p className="hint">This box runs ClamAV only. Add LMD for its web-focused signatures (PHP shells, injected malware ClamAV misses) and the incremental <code>/home</code> scan. It reuses the running clamd, so no extra daemon. Fresh installs get it automatically.</p>
            </div>
            <button disabled={!!loading} onClick={installLmd}><Shield size={14}/> Add Linux Malware Detect</button>
          </div>
        </div>}

        {mwActive && mw.lmd_installed && <div className="info-box">
          <div className="malware-scan-head">
            <div>
              <strong>Real-time protection {mw.realtime_enabled && !mw.realtime_active
                ? <span className="badge danger">Not running</span>
                : <span className={mw.realtime_enabled ? 'badge ok' : 'badge'}>{mw.realtime_enabled ? 'On' : 'Off'}</span>}</strong>
              <p className="hint">Watches every file under <code>/home</code> and scans new/changed files within seconds — instead of only on schedule. Costs RAM per watched file; hits are surfaced, not auto-quarantined.</p>
              {mw.realtime_enabled && !mw.realtime_active && <p className="hint" style={{color:'var(--danger)'}}>
                Turned on here, but the monitor service is not running — nothing is being watched right now.
                Turn it off and on again to restart it.
              </p>}
            </div>
            {mw.realtime_enabled
              ? <button className="danger" disabled={!!loading} onClick={() => toggleMalwareRealtime(false)}>Turn off</button>
              : <button disabled={!!loading} onClick={() => toggleMalwareRealtime(true)}><Shield size={14}/> Turn on</button>}
          </div>
        </div>}

        {mwInstalled && <div className="info-box malware-scan-panel">
          {!mw.clamd_running && <div className="malware-daemon-warning">
            <p className="hint">ClamAV daemon is not running. Start it to enable scanning.</p>
            <button disabled={!!loading} onClick={startClamavDaemon}><Shield size={14}/> Start ClamAV</button>
          </div>}
          {mw.clamd_running && <div className="malware-scan-runner">
            <div className="malware-scan-head">
              <div>
                <strong>Run a scan now</strong>
                <p className="hint">One website, every website (<code>/home</code>), or the whole server (<code>/</code>). Scheduled scans are set up further down.</p>
              </div>
              <button className="secondary" disabled={!!loading} onClick={loadMalwareScanJobs}><RefreshCw size={14}/> History</button>
            </div>
            <div className="malware-scan-controls">
              <select value={scanTargetWebsiteId} onChange={e => { setScanTargetWebsiteId(e.target.value); setScanResults(null); setScanJob(null); }}>
                <option value="">-- Select target --</option>
                <option value="all">All websites (/home)</option>
                <option value="system">Full server (/)</option>
                {websites.map(w => <option key={w.id} value={w.id}>{w.domain}</option>)}
              </select>
              <button disabled={!!loading || scanRunning || !scanTargetWebsiteId} onClick={runMalwareScan}>
                {scanRunning || scanLoading ? <><RefreshCw size={14} className="spin"/> Scanning...</> : <><Search size={14}/> Scan Now</>}
              </button>
            </div>
          </div>}
          {scanJobs.length > 0 && <div className="scan-history-wrap">
            <div className="scan-history-head">
              <strong>Scan history</strong>
              <span>{scanJobs.length} saved</span>
            </div>
            <div className="scan-history-list">
              {scanJobs.slice(0, 8).map(job => <button
                key={job.job_id}
                className={`scan-history-item ${job.status}${activeScanJob.job_id === job.job_id ? ' active' : ''}`}
                onClick={() => showMalwareScanJob(job)}
                disabled={!!loading}
                type="button"
              >
                <Clock size={14}/>
                <span className="scan-history-main">
                  <strong>{scanJobTitle(job)}</strong>
                  <small>{scanJobMeta(job)}</small>
                </span>
                <span className={scanJobBadgeClass(job)}>{job.status}</span>
              </button>)}
            </div>
          </div>}
          {(scanJob || scanResults) && <div className="scan-status-panel">
            <div className="progress-bar">
              <div className="progress-bar-fill" style={{width: `${Number(activeScanJob.progress_percent) || 0}%`}} />
            </div>
            <div className="scan-status-summary">
              <span><strong>Progress</strong>{Number(activeScanJob.progress_percent) || 0}%</span>
              <span><strong>Files scanned</strong>{activeScanJob.scanned || 0}/{activeScanJob.total_files || activeScanJob.scanned || 0}</span>
              <span><strong>Threats found</strong>{activeScanJob.infected > 0
                ? <span className="badge danger">{activeScanJob.infected}</span>
                : <span className="badge ok">0</span>}
              </span>
              <span><strong>Errors</strong>{activeScanJob.errors || 0}</span>
            </div>
            {activeScanJob.message && <p className="hint">{activeScanJob.message}</p>}
            {activeScanJob.threats && activeScanJob.threats.length > 0 && <div className="scan-threat-list">
              {activeScanJob.threats.map((t, i) => <div key={i} className="scan-threat-item">
                <strong>{t.signature}</strong>
                <span>{t.domain ? `${t.domain}: ` : ''}{t.path}</span>
                {(t.quarantined || quarantine.some(q => q.original_path === t.path))
                  ? <span className="badge ok">Quarantined</span>
                  : <button className="secondary-light" disabled={!!loading} onClick={() => quarantineThreat(t.path, t.signature)}>Quarantine</button>}
              </div>)}
            </div>}
            {activeScanJob.log && activeScanJob.log.length > 0 && <pre className="malware-scan-log">{activeScanJob.log.join('\n')}</pre>}
          </div>}
        </div>}

        {mwActive && <div className="info-box">
          <div className="malware-scan-head">
            <div>
              <strong>Quarantine{quarantine.length > 0 && <span className="badge" style={{marginLeft:6}}>{quarantine.length}</span>}</strong>
              <p className="hint">
                {mw.auto_quarantine
                  ? 'A file a scan flags as malware is moved here automatically and stops being served at once. Restore anything that was a false positive.'
                  : 'Auto-quarantine is off — scans only report threats. Turn it on below, or use “Quarantine” on a scan result to move a file here manually.'}
              </p>
            </div>
            <button className="secondary" disabled={!!loading} onClick={loadQuarantine}><RefreshCw size={14}/> Refresh</button>
          </div>
          <label className="check-line" style={{marginBottom: 12}}>
            <input type="checkbox" checked={!!mw.auto_quarantine} disabled={!!loading}
              onChange={e => toggleAutoQuarantine(e.target.checked)} />
            Automatically quarantine detected malware
          </label>
          {quarantine.length === 0
            ? <div className="quarantine-empty"><CheckCircle size={16}/> Nothing in quarantine{mw.auto_quarantine ? ' — flagged files will show up here for review.' : '.'}</div>
            : <div className="quarantine-list">
                {quarantine.map(q => {
                  const m = /^\/home\/[^/]+\/([^/]+)\/(.+)$/.exec(q.original_path || '');
                  return <div key={q.id} className="quarantine-item">
                    <div className="quarantine-info">
                      <div className="quarantine-line">
                        <span className="badge danger">{q.signature || 'flagged file'}</span>
                        {m && <span className="quarantine-site">{m[1]}</span>}
                        <span className="quarantine-meta">{scanJobStamp({ finished_at: q.quarantined_at })} · {formatBytes(q.size || 0)}</span>
                      </div>
                      <code className="quarantine-path" title={q.original_path}>{q.original_path}</code>
                    </div>
                    <div className="quarantine-actions">
                      <button className="secondary-light" disabled={!!loading} onClick={() => restoreQuarantine(q.id, q.original_path)}><RotateCcw size={13}/> Restore</button>
                      <button className="danger-light" disabled={!!loading} onClick={() => deleteQuarantine(q.id, q.original_path)}><Trash2 size={13}/> Delete</button>
                    </div>
                  </div>;
                })}
              </div>}
        </div>}
      </section>}

      {isAdmin && [
        {
          scope: 'system',
          title: 'Full server scan',
          root: '/',
          intro: 'Walks the whole VPS as root, so malware parked in /tmp, /root or a home folder outside public_html is found too. Skips /proc, /sys, /dev, /run, /snap, the ClamAV signature database and panel backup archives. The first run can take hours; best run weekly or monthly.',
        },
        {
          scope: 'web',
          title: 'Website scan (/home)',
          root: '/home',
          intro: 'Scans every website’s files under /home. With Linux Malware Detect installed this runs incrementally (only files changed in the last few days) between full runs, so a daily schedule stays cheap.',
        },
      ].map(({ scope, title, root, intro }) => {
        const sched = malwareSchedules[scope] || {};
        const setField = (patch) => setMalwareSchedules(prev => ({ ...prev, [scope]: { ...prev[scope], ...patch } }));
        return <section className="section" key={scope}>
          <div className="section-title">
            <div>
              <h2>{title}</h2>
              <p className="hint">{intro}</p>
            </div>
            <button className="secondary" disabled={!!loading} onClick={loadMalwareSchedule}><RefreshCw size={14}/> Refresh</button>
          </div>
          <div className="info-box">
            <strong>Scheduled scan <code>{root}</code></strong>
            <p className="hint">Runs automatically on the server clock, even when nobody is logged in to the panel. Use “Run a scan now” above for an on-demand scan.</p>
            <div className="firewall-form">
              <label><span>Enabled</span>
                <select value={sched.enabled ? 'on' : 'off'} onChange={e => setField({ enabled: e.target.value === 'on' })}>
                  <option value="off">Off</option>
                  <option value="on">On</option>
                </select>
              </label>
              <label><span>Frequency</span>
                <select value={sched.frequency || 'weekly'} onChange={e => setField({ frequency: e.target.value })}>
                  <option value="hourly">Hourly</option>
                  <option value="daily">Daily</option>
                  <option value="weekly">Weekly</option>
                  <option value="monthly">Monthly</option>
                </select>
              </label>
              {sched.frequency === 'weekly' && <label><span>Day of week</span>
                <select value={Number(sched.weekday) || 0} onChange={e => setField({ weekday: Number(e.target.value) })}>
                  {WEEKDAY_LABELS.map((label, index) => <option key={label} value={index}>{label}</option>)}
                </select>
              </label>}
              {sched.frequency === 'monthly' && <label><span>Day of month</span>
                <input type="number" min="1" max="28" value={Number(sched.day) || 1} onChange={e => setField({ day: Number(e.target.value) })} />
              </label>}
              {sched.frequency !== 'hourly' && <label><span>Hour</span>
                <input type="number" min="0" max="23" value={Number(sched.hour) || 0} onChange={e => setField({ hour: Number(e.target.value) })} />
              </label>}
              <label><span>Minute</span>
                <input type="number" min="0" max="59" value={Number(sched.minute) || 0} onChange={e => setField({ minute: Number(e.target.value) })} />
              </label>
              <button disabled={!!loading} onClick={() => saveMalwareSchedule(scope)}><Clock size={14}/> Save schedule</button>
            </div>
            {sched.last_run_at && <p className="hint" style={{marginTop:8}}>
              Last scheduled run: {sched.last_run_at} — <span className={sched.last_status === 'infected' ? 'badge danger' : sched.last_status === 'error' ? 'badge bad' : 'badge ok'}>{sched.last_status || 'unknown'}</span> {sched.last_message || ''}
            </p>}
          </div>
        </section>;
      })}
    </>;
  }

  function renderPanelSettings() {
    if (!isAdmin) return <section className="section"><h2>Settings</h2><p className="hint">No permission.</p></section>;
    return <>
      <section className="section">
        <div className="section-title">
          <div><h2>Panel settings</h2><p className="hint">Branding and hostname.</p></div>
          <button disabled={!!loading} onClick={loadPanelSettings}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="panel-settings-grid panel-settings-compact">
          <label><span>Panel name</span><input value={panelSettingsForm.app_name} onChange={e => setPanelSettingsForm(prev => ({ ...prev, app_name: e.target.value }))} placeholder="OPanel Enterprise" /></label>
          <label><span>Panel hostname</span><input value={panelSettingsForm.panel_hostname} onChange={e => setPanelSettingsForm(prev => ({ ...prev, panel_hostname: e.target.value }))} placeholder="panel.domain.com" /></label>
          <label className="check-line panel-ssl-status"><input type="checkbox" checked={!!panelSettingsForm.ssl_enabled} onChange={e => setPanelSettingsForm(prev => ({ ...prev, ssl_enabled: e.target.checked }))} /> Panel SSL</label>
          <button disabled={!!loading || !panelSettingsForm.app_name || !panelSettingsForm.panel_hostname} onClick={savePanelSettings}><SettingsIcon size={14}/> Save settings</button>
        </div>
      </section>
      <section className="section">
        <div className="section-title">
          <div>
            <h2>Server network</h2>
            <p className="hint">Addresses this server answers on. Detected live, so an IPv6 block added later shows up here.</p>
          </div>
          <button disabled={!!loading} onClick={loadNetworkStatus}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="info-box">
          <div className="network-address-row">
            <strong>IPv4</strong>
            {(networkStatus.ipv4 || []).length > 0
              ? (networkStatus.ipv4 || []).map(address => <code key={address}>{address}</code>)
              : <span className="hint">None detected</span>}
          </div>
          <div className="network-address-row">
            <strong>IPv6</strong>
            {(networkStatus.ipv6 || []).length > 0
              ? (networkStatus.ipv6 || []).map(address => <code key={address}>{address}</code>)
              : <span className="hint">None detected</span>}
            {networkStatus.ipv6_available
              ? (networkStatus.ipv6_enabled ? <span className="badge ok">Enabled</span> : <span className="badge">Disabled</span>)
              : <span className="badge warn">Not available</span>}
          </div>
          <div className="actions" style={{marginTop:12}}>
            {networkStatus.ipv6_enabled
              ? <button className="danger" disabled={!!loading} onClick={() => toggleIpv6(false)}>Disable IPv6</button>
              : <button disabled={!!loading || !networkStatus.ipv6_available} onClick={() => toggleIpv6(true)}><Network size={14}/> Enable IPv6</button>}
          </div>
          <p className="hint" style={{marginTop:8}}>
            {networkStatus.ipv6_available
              ? 'Websites and the panel listen on both protocols while this is on. Point an AAAA record at the address above.'
              : 'No global IPv6 address is configured on this server yet. Add one at your provider, then refresh.'}
          </p>
        </div>
      </section>
      <section className="section">
        <div className="section-title">
          <div><h2>Brand assets</h2><p className="hint">Upload PNG, JPG, WEBP, or ICO files up to 1 MB.</p></div>
        </div>
        <div className="brand-asset-grid">
          <div className="brand-asset-card">
            <div className="brand-preview">{renderBrandMark('settings-brand-mark')}</div>
            <label><span>Logo</span><input type="file" accept="image/png,image/jpeg,image/webp,image/x-icon" onChange={e => setPanelLogoFile(e.target.files?.[0] || null)} /></label>
            <button disabled={!!loading || !panelLogoFile} onClick={() => uploadPanelAsset('logo')}><Upload size={14}/> Upload logo</button>
          </div>
          <div className="brand-asset-card">
            <div className="brand-preview favicon-preview">{panelSettings.favicon_url ? <img src={panelSettings.favicon_url} alt="" /> : <Image size={28}/>}</div>
            <label><span>Favicon</span><input type="file" accept="image/png,image/jpeg,image/webp,image/x-icon" onChange={e => setPanelFaviconFile(e.target.files?.[0] || null)} /></label>
            <button disabled={!!loading || !panelFaviconFile} onClick={() => uploadPanelAsset('favicon')}><Upload size={14}/> Upload favicon</button>
          </div>
        </div>
      </section>
      <section className="section">
        <div className="section-title">
          <div><h2>API Tokens</h2><p className="hint">Provisioning tokens for WHMCS or external billing systems.</p></div>
          <button disabled={!!loading} onClick={loadApiTokens}><RefreshCw size={14}/> Refresh</button>
        </div>
        {createdToken && <div className="token-created-notice">
          <p><strong>Token created!</strong> Copy it now — it will not be shown again.</p>
          <div className="token-copy-row">
            <code>{createdToken.token}</code>
            <button className="mini" onClick={() => { copyToClipboard(createdToken.token); setNotice('Copied to clipboard.'); }}><Copy size={14}/> Copy</button>
          </div>
          <button className="mini secondary-light" onClick={() => setCreatedToken(null)}>Dismiss</button>
        </div>}
        <div className="token-create-form">
          <label><span>Name</span><input value={apiTokenForm.name} onChange={e => setApiTokenForm(prev => ({ ...prev, name: e.target.value }))} placeholder="WHMCS Production" /></label>
          <label><span>Expires (days)</span><input type="number" min="1" max="3650" value={apiTokenForm.expires_days} onChange={e => setApiTokenForm(prev => ({ ...prev, expires_days: parseInt(e.target.value) || 365 }))} /></label>
          <label><span>IP allowlist</span><input value={apiTokenForm.ip_allowlist} onChange={e => setApiTokenForm(prev => ({ ...prev, ip_allowlist: e.target.value }))} placeholder="Optional, comma-separated" /></label>
          <button disabled={!!loading || !apiTokenForm.name.trim()} onClick={createApiToken}><Plus size={14}/> Create token</button>
        </div>
        {apiTokens.length === 0 && <p className="hint">No API tokens yet.</p>}
        {apiTokens.length > 0 && <div className="table">
          {apiTokens.map(t => <div className="row" key={t.id}>
            <div className="token-info">
              <strong>{t.name}</strong>
              <small>{t.scopes.join(', ')}</small>
              <small>Prefix: {t.prefix}... | Created: {t.created_at ? new Date(t.created_at).toLocaleDateString() : '—'}{t.last_used_at ? ` | Last used: ${new Date(t.last_used_at).toLocaleDateString()}` : ''}</small>
            </div>
            <span className="badge ok">Active</span>
            <button className="mini danger" disabled={!!loading} onClick={() => deleteApiToken(t)}><Trash2 size={14}/> Delete</button>
          </div>)}
        </div>}
      </section>
    </>;
  }

  function renderUsers() {
    if (!isAdmin) return <section className="section"><h2>Users</h2><p className="hint">No permission.</p></section>;
    return <>
      <section className="section">
        <div className="section-title">
          <div><h2>Panel Users</h2><p className="hint">Manage panel accounts, hosting packages, and create new users.</p></div>
          <button disabled={!!loading} onClick={loadUsers}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="tab-bar">
          <button className={usersTab === 'list' ? 'tab active' : 'tab'} onClick={() => setUsersTab('list')}><Users size={14}/> List Users</button>
          <button className={usersTab === 'packages' ? 'tab active' : 'tab'} onClick={() => setUsersTab('packages')}><PackageOpen size={14}/> Packages</button>
          <button className={usersTab === 'add' ? 'tab active' : 'tab'} onClick={() => setUsersTab('add')}><Plus size={14}/> Add User</button>
        </div>
      </section>
      {usersTab === 'list' && renderUsersListTab()}
      {usersTab === 'packages' && renderUsersPackagesTab()}
      {usersTab === 'add' && renderUsersAddTab()}
    </>;
  }

  function renderUsersListTab() {
    return <section className="section">
      {users.length === 0 && <EmptyState icon={Users} message="No users found." />}
      <div className="table">
        {users.map(user => <div className="row user-row" key={user.id}>
          <div className="user-main"><strong>{user.username}</strong><small>{user.email}</small></div>
          <span className="badge">{roleLabel(user.role)}</span>
          <span className={`badge ${user.is_active ? 'ok' : 'warn'}`}>{user.is_active ? 'Active' : 'Suspended'}</span>
          <span className="user-metric"><HardDrive size={13}/>{storageUsageText(user)}</span>
          <div className="row-actions">
            <button className="mini secondary-light" disabled={!!loading} onClick={() => startEditingUser(user)}><Pencil size={14}/> Edit</button>
            <button className="mini secondary-light" disabled={!!loading} onClick={() => quickLoginUser(user)}><LogIn size={14}/> Login as</button>
            {user.id !== currentUser?.id && <button className={`mini ${user.is_active ? 'danger' : 'secondary-light'}`} disabled={!!loading} onClick={() => toggleUserActive(user)}>{user.is_active ? <><Ban size={14}/> Suspend</> : <><CheckCircle size={14}/> Unsuspend</>}</button>}
            {user.totp_enabled && user.id !== currentUser?.id && <button className="mini secondary-light" disabled={!!loading} onClick={() => resetUserTwoFactor(user)}>Reset 2FA</button>}
            {user.id !== currentUser?.id && <button className="mini danger" disabled={!!loading} onClick={() => deletePanelUser(user)}><Trash2 size={14}/></button>}
          </div>
          {editingUser?.id === user.id && <div className="user-edit-panel">
            <div className="user-edit-heading">
              <div><strong>Edit {user.username}</strong><small>
                {user.id === currentUser?.id ? 'Role is locked for the active admin session.' : 'Role changes sign the user out of existing sessions.'}
                {editingUserForm.role === 'admin' ? ' Admin accounts bypass website and storage limits.' : ''}
              </small></div>
              <button className="user-edit-close secondary-light" onClick={cancelEditingUser} aria-label="Close user editor" title="Close user editor"><X size={16}/></button>
            </div>
            <div className="user-edit-grid">
              <label><span>Email</span><input type="email" value={editingUserForm.email} onChange={e => setEditingUserForm(prev => ({ ...prev, email: e.target.value }))} /></label>
              <label><span>Role</span><select value={editingUserForm.role} disabled={user.id === currentUser?.id} onChange={e => setEditingUserForm(prev => ({ ...prev, role: e.target.value }))}>
                <option value="end_user">End user</option><option value="admin">Admin</option>
              </select></label>
              <label><span>Package</span><select value={editingUserForm._planId || ''} onChange={e => {
                const planId = e.target.value;
                const plan = plans.find(p => String(p.id) === planId);
                if (plan) setEditingUserForm(prev => ({ ...prev, _planId: planId, website_limit: plan.website_limit, storage_limit_mb: plan.storage_limit_mb }));
                else setEditingUserForm(prev => ({ ...prev, _planId: '' }));
              }}>
                <option value="">Custom</option>
                {plans.filter(p => p.active).map(p => <option key={p.id} value={p.id}>{p.name} ({p.website_limit} sites, {p.storage_limit_mb > 0 ? `${p.storage_limit_mb} MB` : 'unlimited'})</option>)}
              </select></label>
              <label><span>Site limit</span><input type="number" min="0" max="1000" value={editingUserForm.website_limit} onChange={e => setEditingUserForm(prev => ({ ...prev, website_limit: e.target.value, _planId: '' }))} /></label>
              <label><span>Disk limit (MB) <em>0 = unlimited</em></span><input type="number" min="0" max="1048576" value={editingUserForm.storage_limit_mb} onChange={e => setEditingUserForm(prev => ({ ...prev, storage_limit_mb: e.target.value, _planId: '' }))} /></label>
              <label><span>New password <small>(leave empty to keep)</small></span><input type="password" value={editingUserForm._password || ''} onChange={e => setEditingUserForm(prev => ({ ...prev, _password: e.target.value }))} placeholder="Min 12 characters" /></label>
            </div>
            <div className="user-edit-actions">
              <button className="secondary-light" onClick={cancelEditingUser}>Cancel</button>
              <button disabled={!!loading || !editingUserForm.email.trim()} onClick={updatePanelUser}><Save size={14}/> Save changes</button>
            </div>
          </div>}
        </div>)}
      </div>
      <div className="section" style={{marginTop:16}}>
        <h2>Assign domain to user</h2>
        <div className="assign-row">
          <select value={assignWebsiteId} onChange={e => setAssignWebsiteId(e.target.value)}>
            <option value="">Select domain</option>
            {websites.map(site => <option key={site.id} value={site.id}>{site.domain}</option>)}
          </select>
          <select value={assignUserId} onChange={e => setAssignUserId(e.target.value)}>
            <option value="">Select user</option>
            {users.map(user => <option key={user.id} value={user.id}>{user.username} ({roleLabel(user.role)})</option>)}
          </select>
          <button disabled={!assignWebsiteId || !assignUserId || !!loading} onClick={assignDomainToUser}>Assign</button>
        </div>
      </div>
    </section>;
  }

  function renderUsersPackagesTab() {
    return <>
      <section className="section">
        <div className="section-title">
          <div><h2>Hosting Packages</h2><p className="hint">Manage provisioning plans for WHMCS and billing systems.</p></div>
          <button disabled={!!loading} onClick={loadPlans}><RefreshCw size={14}/> Refresh</button>
        </div>
        <div className="token-create-form">
          <label><span>Name</span><input value={newPlan.name} onChange={e => { const name = e.target.value; setNewPlan(prev => ({ ...prev, name, slug: name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') })); }} placeholder="Starter" /></label>
          <label><span>Sites</span><input type="number" min="0" value={newPlan.website_limit} onChange={e => setNewPlan(prev => ({ ...prev, website_limit: parseInt(e.target.value) || 0 }))} /></label>
          <label><span>Disk (MB) <em>0 = unlimited</em></span><input type="number" min="0" value={newPlan.storage_limit_mb} onChange={e => setNewPlan(prev => ({ ...prev, storage_limit_mb: parseInt(e.target.value) || 0 }))} /></label>
          <button disabled={!!loading || !newPlan.name.trim()} onClick={createPlan}><Plus size={14}/> Add</button>
        </div>
        {plans.length === 0 && <p className="hint">No packages yet. Create one above.</p>}
        {plans.length > 0 && <div className="table">
          {plans.map(plan => <div className="row" key={plan.id}>
            <div className="token-info">
              <strong>{plan.name}</strong>
              <small>{plan.website_limit} site{plan.website_limit !== 1 ? 's' : ''} | {plan.storage_limit_mb > 0 ? (plan.storage_limit_mb >= 1024 ? `${(plan.storage_limit_mb/1024).toFixed(plan.storage_limit_mb % 1024 ? 1 : 0)} GB` : `${plan.storage_limit_mb} MB`) : 'Unlimited disk'}</small>
            </div>
            <span className={plan.active ? 'badge ok' : 'badge'}>{plan.active ? 'Active' : 'Inactive'}</span>
            <div className="row-actions">
              <button className="mini secondary-light" onClick={() => startEditingPlan(plan)}><Pencil size={14}/> Edit</button>
              <button className="mini danger" onClick={() => deletePlanItem(plan)}><Trash2 size={14}/></button>
            </div>
            {editingPlan?.id === plan.id && <div className="user-edit-panel">
              <div className="user-edit-heading">
                <strong>Edit {plan.name}</strong>
                <button className="user-edit-close secondary-light" onClick={() => setEditingPlan(null)}><X size={16}/></button>
              </div>
              <div className="user-edit-grid">
                <label><span>Name</span><input value={editingPlanForm.name} onChange={e => setEditingPlanForm(prev => ({ ...prev, name: e.target.value }))} /></label>
                <label><span>Sites</span><input type="number" min="0" value={editingPlanForm.website_limit} onChange={e => setEditingPlanForm(prev => ({ ...prev, website_limit: parseInt(e.target.value) || 0 }))} /></label>
                <label><span>Disk (MB) <em>0 = unlimited</em></span><input type="number" min="0" value={editingPlanForm.storage_limit_mb} onChange={e => setEditingPlanForm(prev => ({ ...prev, storage_limit_mb: parseInt(e.target.value) || 0 }))} /></label>
                <label className="check-line"><input type="checkbox" checked={editingPlanForm.active} onChange={e => setEditingPlanForm(prev => ({ ...prev, active: e.target.checked }))} /> Active</label>
              </div>
              <div className="user-edit-actions">
                <button className="secondary-light" onClick={() => setEditingPlan(null)}>Cancel</button>
                <button disabled={!!loading} onClick={updatePlan}><Save size={14}/> Save</button>
              </div>
            </div>}
          </div>)}
        </div>}
      </section>
    </>;
  }

  function renderUsersAddTab() {
    return <>
      <section className="section">
        <div className="section-title">
          <div><h2>Add panel user</h2><p className="hint">Panel username is also the Linux user. Select a package to auto-fill limits.</p></div>
        </div>
        <div className="user-create-card">
          <label><span>Username</span><input value={newUser.username} onChange={e => setNewUser(prev => ({ ...prev, username: e.target.value.toLowerCase() }))} placeholder="johndoe" /></label>
          <label><span>Email</span><input value={newUser.email} onChange={e => setNewUser(prev => ({ ...prev, email: e.target.value }))} placeholder="user@domain.com" /></label>
          <label><span>Password</span><input value={newUser.password} onChange={e => setNewUser(prev => ({ ...prev, password: e.target.value }))} placeholder="Min 12 characters" type="password" /></label>
          <label><span>Role</span><select value={newUser.role} onChange={e => setNewUser(prev => ({ ...prev, role: e.target.value }))}>
            <option value="end_user">End user</option><option value="admin">Admin</option>
          </select></label>
          <label><span>Package</span><select value={newUser._planId || ''} onChange={e => {
            const planId = e.target.value;
            const plan = plans.find(p => String(p.id) === planId);
            if (plan) {
              setNewUser(prev => ({ ...prev, _planId: planId, website_limit: plan.website_limit, storage_limit_mb: plan.storage_limit_mb }));
            } else {
              setNewUser(prev => ({ ...prev, _planId: '' }));
            }
          }}>
            <option value="">Custom</option>
            {plans.filter(p => p.active).map(p => <option key={p.id} value={p.id}>{p.name} ({p.website_limit} sites, {p.storage_limit_mb > 0 ? `${p.storage_limit_mb} MB` : 'unlimited'})</option>)}
          </select></label>
          <label><span>Site limit</span><input type="number" value={newUser.website_limit} onChange={e => setNewUser(prev => ({ ...prev, website_limit: e.target.value, _planId: '' }))} /></label>
          <label><span>Disk (MB) <em>0 = unlimited</em></span><input type="number" value={newUser.storage_limit_mb} onChange={e => setNewUser(prev => ({ ...prev, storage_limit_mb: e.target.value, _planId: '' }))} /></label>
          <button disabled={!!loading || !newUser.username || !newUser.password} onClick={createUser}><Plus size={14}/> Create user</button>
        </div>
      </section>
    </>;
  }

  function renderStandaloneEditor() {
    const editorLineCount = Math.max(1, String(fileContent || '').split('\n').length);
    const editorMode = editorLanguage(filePath);
    const siteLabel = currentSite?.domain || (selectedWebsiteId ? `Website #${selectedWebsiteId}` : 'Website');
    return <main className="standalone-editor-page">
      <header className="standalone-editor-top">
        <div className="standalone-editor-title">
          <strong>{filePath || 'No file selected'}</strong>
          <span>{siteLabel}</span>
        </div>
        <div className="standalone-editor-actions">
          <span className="editor-chip">{editorMode}</span>
          <span className="editor-chip">{editorLineCount} line(s)</span>
          <span className="editor-chip">Ln {editorCursor.line}, Col {editorCursor.column}</span>
          <button disabled={!selectedWebsiteId || !!loading} onClick={() => readFile(filePath)}><RefreshCw size={14}/> Reload</button>
          <button disabled={!selectedWebsiteId || !!loading} onClick={writeFile}>Save</button>
          <button disabled={!selectedWebsiteId || !filePath || !!loading} onClick={() => downloadFile(filePath)}><Download size={14}/></button>
          <button className="secondary-light" onClick={() => window.close()}><X size={14}/> Close</button>
        </div>
      </header>
      {loading && <div className="loading">{loading}</div>}
      {renderNotifications()}
      <section className="standalone-editor-body">
        <CodeEditor
          value={fileContent}
          mode={editorMode}
          disabled={!selectedWebsiteId}
          theme={theme}
          onChange={setFileContent}
          onCursorChange={setEditorCursor}
        />
      </section>
    </main>;
  }

  function renderPage() {
    if (page === 'websites') return renderWebsites();
    if (page === 'ssl') return renderSsl();
    if (page === 'databases') return renderDatabases();
    if (page === 'cron') return renderCron();
    if (page === 'files') return renderFiles();
    if (page === 'backups') return renderBackups();
    if (page === 'security') return renderSecurity();
    if (page === 'malware') return renderMalwareScanner();
    if (page === 'php') return renderPhpConfig();
    if (page === 'firewall') return renderFirewall();
    if (page === 'waf') return renderWaf();
    if (page === 'wafLogs') return renderWafAccessLogs();
    if (page === 'updates') return renderUpdates();
    if (page === 'services') return renderServices();
    if (page === 'settings') return renderPanelSettings();
    if (page === 'users') return renderUsers();
    return renderDashboard();
  }

  // Login screen
  if (bootstrapping) {
    return <main className="login-page">
      <button type="button" className="theme-toggle-btn" onClick={toggleTheme} aria-label="Toggle dark mode" title="Toggle dark mode">{theme === 'dark' ? <Sun size={16}/> : <Moon size={16}/>}</button>
      <section className="login-card">
        <div className="login-brand">{renderBrandMark('login-brand-mark')}<div><p className="eyebrow">{panelSettings.app_name || 'opanel-ent'}</p><h1>Loading…</h1></div></div>
      </section>
    </main>;
  }

  if (!isAuthenticated) {
    return <main className="login-page">
      <button type="button" className="theme-toggle-btn" onClick={toggleTheme} aria-label="Toggle dark mode" title="Toggle dark mode">{theme === 'dark' ? <Sun size={16}/> : <Moon size={16}/>}</button>
      <section className="login-card">
        <div className="login-brand">
          {renderBrandMark('login-brand-mark')}
          <div>
            <p className="eyebrow">Server Management Panel</p>
            <h1>{panelSettings.app_name || 'opanel-ent'}</h1>
            <p className="hint">Manage websites, databases, backups, SSL, and services.</p>
          </div>
        </div>
        <div className="login-form">
          <input value={username} onChange={e => setUsername(e.target.value)} placeholder="Username" autoComplete="username" />
          <input value={password} onChange={e => setPassword(e.target.value)} placeholder="Password" type="password" autoComplete="current-password" onKeyDown={e => { if (e.key === 'Enter') login(); }} />
          {needsTwoFactor && <input value={otpCode} onChange={e => setOtpCode(e.target.value)} placeholder="Authentication code" inputMode="numeric" autoComplete="one-time-code" onKeyDown={e => { if (e.key === 'Enter') login(); }} />}
          <button disabled={!!loading || !username || !password} onClick={login}>{loading ? 'Logging in...' : 'Login'}</button>
        </div>
      </section>
      {renderNotifications()}
    </main>;
  }

  if (standaloneEditor) return renderStandaloneEditor();

  const ActiveIcon = activeNavItem?.[2] || Home;

  return <main className="app-shell">
    <section className="layout">
      {mobileMenuOpen && <div className="mobile-nav-backdrop" onClick={() => setMobileMenuOpen(false)} aria-hidden="true"></div>}
      <aside className={`sidebar ${mobileMenuOpen ? 'open' : ''}`} role="navigation" aria-label="Main navigation">
        <div className="sidebar-head">
          <div className="sidebar-brand">
            {renderBrandMark()}
            <div>
              <strong>{panelSettings.app_name || 'opanel-ent'}</strong>
              <small>Server Panel</small>
            </div>
          </div>
          <button className="sidebar-close" onClick={() => setMobileMenuOpen(false)} aria-label="Close menu"><X size={18}/></button>
        </div>
        <nav className="sidebar-nav">
          {mainNavItems.map(([key, label, Icon]) => <button key={key} type="button" className={page === key ? 'active' : ''} onClick={() => navigateToPage(key)} aria-current={page === key ? 'page' : undefined}>
            <Icon size={17}/>{label}
          </button>)}
          <div className={`sidebar-nav-group ${settingsMenuOpen ? 'open' : ''}`}>
            <button className={`sidebar-group-toggle ${settingsIsActive ? 'active' : ''}`} onClick={() => setSettingsMenuOpen(open => !open)} aria-expanded={settingsMenuOpen} aria-controls="settings-submenu">
              <SettingsIcon size={17}/><span>Settings</span><ChevronDown className="sidebar-group-chevron" size={16}/>
            </button>
            {settingsMenuOpen && <div className="sidebar-subnav" id="settings-submenu">
              {settingsNavItems.map(([key, label, Icon]) => <button key={key} type="button" className={page === key ? 'active' : ''} onClick={() => navigateToPage(key)} aria-current={page === key ? 'page' : undefined}>
                <Icon size={16}/>{label}
              </button>)}
            </div>}
          </div>
        </nav>
        {appVersion && <div className="sidebar-version">v{appVersion}</div>}
      </aside>
      <div className="content">
        <section className="topbar">
          <button className="mobile-nav-toggle" onClick={() => setMobileMenuOpen(o => !o)} aria-expanded={mobileMenuOpen} aria-label="Toggle navigation">
            <Menu size={20}/><span><ActiveIcon size={17}/>{activeNavItem?.[1] || 'Menu'}</span>
          </button>
          <div className="page-title">
            <p className="eyebrow">Server Management Panel</p>
            <h1>{activeNavItem?.[1] || panelSettings.app_name || 'opanel-ent'}</h1>
          </div>
          <div className="login logged-in">
            <div className="account-pill"><span>Logged in as</span><strong>{currentUser?.username || username}</strong></div>
            <div className="top-actions">
              <button className="secondary compact-btn icon-only" onClick={toggleTheme} aria-label="Toggle dark mode" title="Toggle dark mode">{theme === 'dark' ? <Sun size={15}/> : <Moon size={15}/>}</button>
              <button className="secondary compact-btn" onClick={openProfileModal} aria-label="Profile settings" title="Profile settings"><KeyRound size={15}/><span className="btn-label">Profile</span></button>
              <button className="secondary compact-btn" onClick={logout} aria-label="Logout" title="Logout"><LogOut size={15}/><span className="btn-label">Logout</span></button>
            </div>
          </div>
        </section>
        <div className="content-body">
          {renderPage()}
          {loading && <div className="loading"><span></span>{loading}</div>}
        </div>
      </div>
    </section>
    {showProfileModal && <div className="modal-overlay" onClick={() => setShowProfileModal(false)}>
      <div className="modal-card" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <h3>Profile Settings</h3>
          <button className="secondary-light" onClick={() => setShowProfileModal(false)}><X size={16}/></button>
        </div>
        <div className="modal-body">
          <div className="modal-section">
            <h4>Email</h4>
            <div className="profile-email-row">
              <input type="email" value={profileForm.email} onChange={e => setProfileForm(prev => ({ ...prev, email: e.target.value }))} placeholder="admin@domain.com" />
              <button disabled={!!loading || !profileForm.email.trim() || profileForm.email === currentUser?.email} onClick={updateMyEmail}><Save size={14}/> Save</button>
            </div>
            <p className="hint">Used for Let's Encrypt SSL notifications and panel communication.</p>
          </div>
          <div className="modal-section">
            <h4>Change Password</h4>
            <label><span>New password</span><input type="password" value={profileForm.password} onChange={e => setProfileForm(prev => ({ ...prev, password: e.target.value }))} placeholder="Min 12 characters" /></label>
            <label><span>Current password</span><input type="password" value={profileForm.current_password} onChange={e => setProfileForm(prev => ({ ...prev, current_password: e.target.value }))} placeholder="Required to confirm" /></label>
            {currentUser?.totp_enabled && <label><span>2FA code</span><input value={profileForm.code} onChange={e => setProfileForm(prev => ({ ...prev, code: e.target.value }))} placeholder="6-digit code" maxLength={6} /></label>}
            <button disabled={!!loading || !profileForm.password || profileForm.password.length < 12} onClick={changeMyPasswordFromProfile}><KeyRound size={14}/> Change password</button>
          </div>
        </div>
      </div>
    </div>}
    {renderNotifications()}
  </main>;
}

createRoot(document.getElementById('root')).render(<App />);
