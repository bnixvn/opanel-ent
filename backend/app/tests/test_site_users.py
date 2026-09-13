import hashlib
from pathlib import Path

import pytest

from app.api import websites
from app.services import site_users

PROJECT_ROOT = Path(__file__).resolve().parents[3]
HELPER_SCRIPT = PROJECT_ROOT / "installer" / "files" / "opanel-ent-helper.sh"
INSTALL_SCRIPT = PROJECT_ROOT / "installer" / "install.sh"
UPDATE_SCRIPT = PROJECT_ROOT / "installer" / "update.sh"


def test_site_php_fpm_socket_is_scoped_to_site_root(tmp_path):
    first_root = tmp_path / "first.test"
    second_root = tmp_path / "second.test"

    first_socket = site_users.site_php_fpm_socket("siteuser", first_root, "8.3")
    second_socket = site_users.site_php_fpm_socket("siteuser", second_root, "8.3")

    first_hash = hashlib.sha256(str(first_root.resolve()).encode("utf-8")).hexdigest()[:12]
    assert first_socket == f"/tmp/lshttpd/opanel-ent-siteuser-{first_hash}-lsphp83.sock"
    assert second_socket != first_socket


def test_site_php_fpm_socket_returns_none_without_php_version(tmp_path):
    assert site_users.site_php_fpm_socket("siteuser", tmp_path, None) is None


def test_php_fpm_socket_rejects_invalid_php_version(tmp_path):
    with pytest.raises(ValueError, match="Invalid PHP version"):
        site_users.site_php_fpm_socket("siteuser", tmp_path, "../8.3")


def test_legacy_user_php_fpm_socket_is_kept_for_callers_without_site_root():
    assert site_users.php_fpm_socket("siteuser", "8.3") == "/tmp/lshttpd/opanel-ent-siteuser-lsphp83.sock"


def test_placeholder_page_for_linux_user_uses_site_file_write(tmp_path, monkeypatch):
    root = tmp_path / "site"
    public = root / "public_html"
    public.mkdir(parents=True)
    calls = []

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(websites.file_manager.shell, "privileged", fake_privileged)
    monkeypatch.setattr(websites.file_manager, "_clear_fastcgi_cache", lambda: None)

    websites._write_placeholder_page("example.test", str(root), "siteuser", "8.3")

    assert calls[0][0] == "site-file-write"
    assert calls[0][1] == ["siteuser", str(root.resolve()), "public_html/index.html"]
    assert "example.test" in calls[0][2]["input"]


def test_import_site_files_uses_privileged_site_import_copy(tmp_path, monkeypatch):
    root = tmp_path / "site"
    root.mkdir()
    staging = tmp_path / "opanel-ent-da-import-xyz" / "example.test"
    staging.mkdir(parents=True)
    calls = []

    def fake_privileged(helper_command, helper_args=None, **kwargs):
        calls.append((helper_command, helper_args, kwargs))
        return type("Result", (), {"returncode": 0, "stdout": "", "stderr": ""})()

    monkeypatch.setattr(site_users.shell, "privileged", fake_privileged)

    site_users.import_site_files(str(root), "public_html", "siteuser", staging)

    assert calls[0][0] == "site-import-copy"
    assert calls[0][1] == ["siteuser", str(root), "public_html", str(staging)]


def test_site_import_copy_helper_rejects_symlinks_and_staging_outside_da_import_tmp():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "site-import-copy)" in helper
    assert "/tmp/opanel-ent-da-import-*" in helper
    assert 'deny "staging source outside expected da-import temp dir' in helper
    assert "os.path.islink(src)" in helper
    assert 'raise ValueError("import destination contains an unsafe symlink")' in helper
    assert 'raise ValueError("import path escapes website root")' in helper
    assert "os.O_NOFOLLOW" in helper
    # The final chown/chmod pass must still run so imported files end up
    # owned by the site's own Linux user, not root.
    import_block = helper.split("site-import-copy)", 1)[1].split("site-archive-extract)", 1)[0]
    assert 'fix_site_tree "$root_target" "$user"' in import_block


def test_panel_linux_users_are_sftp_chroot_only():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    for script_path in (INSTALL_SCRIPT, UPDATE_SCRIPT):
        script = script_path.read_text(encoding="utf-8")
        assert "Match Group opanel-ent-sftp" in script
        assert "ChrootDirectory /home/%u" in script
        assert "ForceCommand internal-sftp -d /" in script
        assert "PermitTTY no" in script
        assert "AllowTcpForwarding no" in script
    assert "--shell /usr/sbin/nologin" in helper
    assert "--shell /bin/bash" not in helper
    assert 'chmod 0711 "$HOME_ROOT"' in helper
    assert 'chown "root:$user" "$home_dir"' in helper
    assert 'chmod 0751 "$home_dir"' in helper
    assert 'usermod -aG "$user" www-data' in helper


def test_panel_tools_ssl_vhosts_enable_http2_for_nginx_1_24():
    # OLS uses its own listener config for HTTP/2 — check helper uses lswsctrl
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "/usr/local/lsws/bin/lswsctrl" in helper


def test_site_permissions_use_standard_wordpress_modes():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    update = UPDATE_SCRIPT.read_text(encoding="utf-8")
    assert 'find "$target" -type d -exec chmod 755 {} +' in helper
    assert 'find "$target" -type f -exec chmod 644 {} +' in helper
    assert 'chown -R "$user:$user" "$target"' in helper
    assert 'harden_site_file "$target" "$user"' in helper
    assert 'install -o "$user" -g "$user" -m 0644' in helper
    assert 'chown -R "$user:$user" "$site_dir"' in update
    assert 'find "$site_dir" -type d -exec chmod 755 {} +' in update
    assert 'find "$site_dir" -type f -exec chmod 644 {} +' in update
    assert 'find "$target" -type d -exec chmod a-s {} +' in helper
    assert 'find "$site_dir" -type d -exec chmod a-s {} +' in update
    assert 'chmod a-s "$target"' in helper
    assert 'find "$target" -type d -exec chmod 2750 {} +' not in helper
    assert 'find "$target" -type f -exec chmod 640 {} +' not in helper
    assert 'find "$site_dir" -type d -exec chmod 2750 {} +' not in update
    assert 'find "$site_dir" -type f -exec chmod 640 {} +' not in update
    assert 'find "$target" -type d -exec chmod u-s {} +' not in helper
    assert 'find "$site_dir" -type d -exec chmod u-s {} +' not in update


def test_ols_server_group_can_read_managed_site_roots():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert 'user                             www-data' in helper
    assert 'group                            opanel-ent-sites' in helper
    assert 'user                             nobody' not in helper
    assert 'group                            nogroup' not in helper
    assert 'install -d -o www-data -g "$opanel_ent_SITES_GROUP" -m 2775 /tmp/lshttpd' in helper


def test_php_upload_tmp_dir_keeps_nginx_readable_group():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "ensure_php_runtime_dirs()" in helper
    assert 'install -d -o "$user" -g "$user" -m 0700 "$upload_dir"' in helper
    assert 'chmod g-s "$upload_dir"' in helper
    assert 'install -d -o "$user" -g "$user" -m 0700 "$sess_dir"' in helper
    update = UPDATE_SCRIPT.read_text(encoding="utf-8")
    assert 'chown "$user:$user" "/var/lib/php/uploads/$user"' in update
    assert 'chmod 0700 "/var/lib/php/uploads/$user"' in update
    assert 'chmod g-s "/var/lib/php/uploads/$user"' in update
    assert 'ensure_php_runtime_dirs "$pool_user"' in helper


def test_firewall_blocklist_ipset_sizes_scale_with_list_size():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "v4_count=\"$(awk" in helper
    assert 'index($0, ":") == 0' in helper
    assert 'index($0, ":") > 0' in helper
    assert 'v4_max=$(( v4_count + v4_count / 4 + 1024 ))' in helper
    assert 'ipset swap "$v4_new" "$BLOCKLIST_IPSET_V4"' in helper
    assert 'maxelem "$v4_max"' in helper
    assert 'maxelem "$v6_max"' in helper


def test_php_fpm_pools_are_auto_tuned_for_vps_size():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "calculate_php_fpm_pool_tuning()" in helper
    assert "php_fpm_total_memory_mb()" in helper
    assert "php_fpm_cpu_count()" in helper
    assert "php_fpm_pool_count()" in helper
    assert "active_pool_divisor * active_pool_divisor < pool_count" in helper
    assert 'php_fpm_set_directive "$pool_file" "pm.max_children" "$PHP_FPM_MAX_CHILDREN"' in helper
    assert 'php_fpm_set_directive "$pool_file" "pm.process_idle_timeout" "${PHP_FPM_PROCESS_IDLE_TIMEOUT}s"' in helper
    assert 'php_fpm_set_directive "$pool_file" "pm.max_requests" "$PHP_FPM_MAX_REQUESTS"' in helper
    assert 'php_fpm_set_directive "$pool_file" "request_terminate_timeout" "${PHP_FPM_REQUEST_TERMINATE_TIMEOUT}s"' in helper
    assert "opanel_ent_PHP_FPM_WORKER_MB" in helper
    assert "opanel_ent_PHP_FPM_MAX_CHILDREN" in helper
    assert "php-fpm-retune)" in helper
    assert "pm.max_children = 8" not in helper


def test_mariadb_is_auto_tuned_for_vps_size():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "calculate_mariadb_tuning()" in helper
    assert "write_mariadb_tuning()" in helper
    assert "mariadb-retune)" in helper
    assert "innodb_buffer_pool_size = ${MARIADB_INNODB_BUFFER_POOL_SIZE}" in helper
    assert "max_connections = ${MARIADB_MAX_CONNECTIONS}" in helper
    assert "table_open_cache = ${MARIADB_TABLE_OPEN_CACHE}" in helper
    assert "ensure_mariadb_slow_log" in helper
    assert "opanel_ent_MARIADB_BUFFER_POOL_SIZE" in helper


def test_openlitespeed_site_runtime_uses_site_user_and_writable_logs():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    install_script = INSTALL_SCRIPT.read_text(encoding="utf-8")
    ctl_script = (PROJECT_ROOT / "installer" / "files" / "opanel-ent-ctl").read_bytes()

    assert not ctl_script.startswith(b"\xef\xbb\xbf")
    assert "ensure_sites_group" in helper
    assert 'install -d -o www-data -g "$opanel_ent_SITES_GROUP" -m 2775 /var/log/openlitespeed' in helper
    assert "chmod g+s /var/log/openlitespeed" in helper
    assert '"    setUIDMode               2",' in helper
    assert "install -d -o www-data -g opanel-ent-sites -m 2775 /var/log/openlitespeed" in install_script
    assert "chmod g+s /var/log/openlitespeed" in install_script


def test_vhost_write_creates_a_site_owned_php_log_dir():
    """PHP's error_log lives in /var/log/openlitespeed/<domain>/, owned by the
    site's Linux user so its PHP process can actually write to it."""
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert 's#^[[:space:]]*docRoot[[:space:]]+/home/([^/]+)/.*#\\1#p' in helper
    assert 'install -d -o "$vhost_site_user" -g "$vhost_site_user" -m 0750 "/var/log/openlitespeed/$safe_domain"' in helper
    # the Error tab reads the PHP log then the OLS server log
    assert "PHP error log" in helper
    assert "OpenLiteSpeed error log" in helper
    # php_error.log has no built-in rotation -> logrotate handles it
    assert "/var/log/openlitespeed/*/php_error.log {" in helper
    assert "/etc/logrotate.d/opanel-ent-sites" in helper


def test_manual_ssl_helper_installs_private_key_outside_web_root():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "install_manual_ssl()" in helper
    assert "remove_manual_ssl()" in helper
    assert 'base="/usr/local/lsws/conf/opanel-ent/ssl/sites/${domain}"' in helper
    assert 'install -m 0640 -o root -g opanel-ent "$tmpdir/privkey.key" "$base/privkey.key"' in helper
    assert "manual-ssl-install)" in helper
    assert "manual-ssl-remove)" in helper


def test_terminal_helper_rejects_paths_outside_user_home():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "require_terminal_path_args()" in helper
    assert "require_terminal_download_args()" in helper
    assert 'deny "terminal path argument is outside panel user home: $arg"' in helper
    assert 'deny "terminal path argument escapes user home: $arg"' in helper
    assert 'deny "terminal URL argument uses local file scheme: $arg"' in helper
    assert 'require_terminal_path_args "$user" "$target" "$@"' in helper
    assert 'require_terminal_download_args "$user" "$target" "$@"' in helper


def test_rm_site_helper_binds_delete_to_user_root_and_deletes_no_follow():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    assert "require_bound_managed_path()" in helper
    assert "delete_no_follow()" in helper
    assert 'target=$(require_bound_managed_path "$user" "$root" "$path")' in helper
    assert "os.path.normpath(sys.argv[1])" in helper
    assert 'root_relative="${normalized_root#${HOME_ROOT}/${user}/}"' in helper
    assert '[[ "$root_relative" == */* ]] && deny "site root must be a direct domain path: $normalized_root"' in helper
    assert '[[ "$target" == "$normalized_root" || "$target_relative" == */* ]] || deny "refusing to operate on a panel user home"' in helper
    assert 'delete_no_follow "$user" "$root" "$target"' in helper
    assert "os.O_NOFOLLOW" in helper
    assert "os.unlink(name, dir_fd=dir_fd)" in helper
    assert 'usage: rm-site <site-user> <site-root> <path>' in helper


def test_update_skips_per_site_refresh_when_nothing_site_facing_changed():
    update = UPDATE_SCRIPT.read_text(encoding="utf-8")
    # fingerprint gate on the files that actually shape per-site output
    assert 'SITE_FP_FILE="/var/lib/opanel-ent/site-refresh.fingerprint"' in update
    assert "site_refresh_fingerprint()" in update
    assert 'backend/app/services/openlitespeed.py' in update
    assert 'backend/app/api/websites.py' in update
    assert "skipping the per-site refresh" in update
    assert "--refresh-sites)" in update
    assert 'FORCE_SITE_REFRESH="${FORCE_SITE_REFRESH:-false}"' in update
    # when the loop does run it defers the OLS/WAF reload to the single
    # ols-sync-main near the end of the script
    assert "waf.sync_website_rules(website, defer_reload=True)" in update
    assert "_rewrite_website_vhost(website, defer_reload=True)" in update
    # the redundant second recursive chown per site is gone from that loop
    loop = update.split("Refreshing managed site config", 1)[1].split("Compiling backend modules", 1)[0]
    assert "site_users.fix_site_permissions(" not in loop
    # the recursive panel-user re-harden is version-gated too
    assert "HARDEN_SITES_VERSION=" in update
    assert 'harden_marker="/var/lib/opanel-ent/harden-sites.version"' in update
    assert 'if [[ "$do_recursive" == 1 ]]; then' in update


def test_the_expensive_tree_reharden_is_gated_separately_from_the_config_rewrite():
    update = UPDATE_SCRIPT.read_text(encoding="utf-8")
    loop = update.split("Refreshing managed site config", 1)[1].split("Compiling backend modules", 1)[0]
    # the recursive fix_site_tree call (via ensure_site_runtime) runs only when
    # re-hardening; the vhost + WAF rewrite always runs when the loop executes
    assert "SITE_HARDEN_VERSION=" in update
    assert 'SITE_HARDEN_MARKER="/var/lib/opanel-ent/site-harden.version"' in update
    assert "if reharden and website.linux_user:" in loop
    assert "site_users.ensure_site_runtime(" in loop
    # a box already hardened by harden_existing_panel_users v2 is seeded, not redone
    assert '"$(cat /var/lib/opanel-ent/harden-sites.version 2>/dev/null || true)" == "2"' in update
    # the phase reports progress instead of sitting frozen at 65%
    assert "opanel-ent-site-progress" in update
    assert "_site_refresh_progress" in update


def test_update_skips_migrations_when_the_schema_is_already_at_head():
    update = UPDATE_SCRIPT.read_text(encoding="utf-8")
    assert "Database schema already at head -- skipping migrations" in update
    assert "get_current_heads()" in update
    assert "from app.core.database import run_migrations; run_migrations()" in update


def test_helper_skips_ols_restart_when_generated_config_is_unchanged():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    # WAF site file
    assert 'if [[ -f "$target" ]] && cmp -s "$tmp" "$target"; then' in helper
    assert "WAF site rules unchanged:" in helper
    # PHP-FPM pool file
    assert 'if [[ -f "$pool_file" ]] && cmp -s "$pool_tmp" "$pool_file"; then' in helper
    # OLS vhost file
    assert 'if [[ -f "$vhost_conf" ]] && cmp -s "$vhost_tmp" "$vhost_conf"; then' in helper
    assert "vhost unchanged:" in helper


def test_archive_extract_helper_skips_symlinks_instead_of_rejecting():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    block = helper.split("site-archive-extract)", 1)[1].split("\nPY\n", 1)[0]
    # the whole-archive rejection is gone
    assert 'raise ValueError("archive links and devices are not allowed")' not in block
    assert 'raise ValueError("archive symlinks are not allowed")' not in block
    # sym / link / dev / fifo members are skipped in both validate and extract
    assert block.count("member.issym() or member.islnk() or member.isdev() or member.isfifo()") >= 2
    assert "skipped_specials += 1" in block
    assert "skipped %d symlink/special" in block


def test_wp_cli_wrapper_disables_jit_for_ioncube_compat():
    helper = HELPER_SCRIPT.read_text(encoding="utf-8")
    # every wp-cli entrypoint turns opcache JIT off (ionCube user-opcode-handler
    # conflict) alongside the existing pcre.jit=0
    for marker in (
        "WP_CLI_PHP_ARGS='-d pcre.jit=0 -d opcache.jit=disable'",
    ):
        assert helper.count(marker) >= 3, marker
    assert "-d pcre.jit=0 -d opcache.jit=disable /usr/local/bin/wp" in helper
    assert "WP_CLI_PHP_ARGS='-d pcre.jit=0'" not in helper
