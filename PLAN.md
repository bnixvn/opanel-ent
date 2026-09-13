# KÃ¡ÂºÂ¿ HoÃ¡ÂºÂ¡ch Fork opanel-ent: OpenLiteSpeed + iptables

## Summary
- Fork `opanel-ent` thÃƒÂ nh `opanel-ent`, Ã„â€˜Ã¡Â»â€¢i toÃƒÂ n bÃ¡Â»â„¢ brand/runtime: `/opt/opanel-ent`, `/var/lib/opanel-ent`, `/etc/opanel-ent`, `opanel-ent-api`, `opanel-ent-helper`, `opanel-ent-update`, user/group `opanel-ent`.
- ChuyÃ¡Â»Æ’n webserver tÃ¡Â»Â« Nginx sang OpenLiteSpeed, dÃƒÂ¹ng LSPHP/LSAPI lÃƒÂ m PHP runtime mÃ¡ÂºÂ·c Ã„â€˜Ã¡Â»â€¹nh.
- Thay UFW bÃ¡ÂºÂ±ng firewall do opanel-ent quÃ¡ÂºÂ£n lÃƒÂ½ trÃ¡Â»Â±c tiÃ¡ÂºÂ¿p bÃ¡ÂºÂ±ng `iptables`/`ip6tables`; URL TXT blocklist dÃƒÂ¹ng `iptables` match qua `ipset` Ã„â€˜Ã¡Â»Æ’ chÃ¡Â»â€¹u Ã„â€˜Ã†Â°Ã¡Â»Â£c danh sÃƒÂ¡ch lÃ¡Â»â€ºn.

## Key Changes
- Installer/update:
  - CÃƒÂ i LiteSpeed repo, `openlitespeed`, `lsphp83`, `lsphp84`, cÃƒÂ¡c extension cÃ¡ÂºÂ§n cho WordPress/PHP, `ols-modsecurity`, `iptables`, `ipset`, `iptables-persistent`, `certbot`.
  - KhÃƒÂ´ng cÃƒÂ i `nginx`, `python3-certbot-nginx`, `ufw`; disable/mask Nginx vÃƒÂ  UFW khi migration Ã„â€˜ÃƒÂ£ pass health check.
  - Detect service OpenLiteSpeed lÃƒÂ  `lsws` hoÃ¡ÂºÂ·c `openlitespeed`, lÃ†Â°u `WEBSERVER_SERVICE` vÃƒÂ o `.env`.

- OpenLiteSpeed:
  - TÃ¡ÂºÂ¡o service layer mÃ¡Â»â€ºi `openlitespeed`/`webserver`, thay renderer Nginx hiÃ¡Â»â€¡n tÃ¡ÂºÂ¡i.
  - QuÃ¡ÂºÂ£n lÃƒÂ½ config dÃ†Â°Ã¡Â»â€ºi `/usr/local/lsws/conf/opanel-ent/`, include tÃ¡Â»Â« `httpd_config.conf`, sinh vhost cho website, aliases, redirects, SSL, logs, rewrite modes.
  - PHP site dÃƒÂ¹ng LSAPI external app trÃ¡Â»Â tÃ¡Â»â€ºi `/usr/local/lsws/lsphpXX/bin/lsphp`; bÃ¡Â»Â cÃ†Â¡ chÃ¡ÂºÂ¿ PHP-FPM pool cho site runtime.
  - phpMyAdmin tiÃ¡ÂºÂ¿p tÃ¡Â»Â¥c cÃƒÂ³ SSO, nhÃ†Â°ng tools vhost chuyÃ¡Â»Æ’n sang OLS context + LSPHP.
  - Certbot Ã„â€˜Ã¡Â»â€¢i sang `certonly --webroot` cho site SSL; panel SSL dÃƒÂ¹ng standalone/webroot vÃƒÂ  deploy hook restart OLS + `opanel-ent-api`.

- WAF/cache/flood:
  - WAF chuyÃ¡Â»Æ’n sang OLS ModSecurity (`ols-modsecurity`), rule files dÃ†Â°Ã¡Â»â€ºi `/usr/local/lsws/conf/opanel-ent/modsec`.
  - FastCGI cache Ã„â€˜Ã¡Â»â€¢i sang LSCache-equivalent; helper `fastcgi-cache-clear` giÃ¡Â»Â¯ alias nhÃ†Â°ng thÃ¡Â»Â±c thi purge cache OLS.
  - HTTP flood giÃ¡Â»Â¯ UI/API hiÃ¡Â»â€¡n tÃ¡ÂºÂ¡i, implement bÃ¡ÂºÂ±ng OLS throttling/rewrite tÃ†Â°Ã†Â¡ng Ã„â€˜Ã†Â°Ã†Â¡ng vÃƒÂ  test theo cÃƒÂ¹ng ngÃ†Â°Ã¡Â»Â¡ng hiÃ¡Â»â€¡n cÃƒÂ³.

- Firewall:
  - GiÃ¡Â»Â¯ API `/firewall/*`, nhÃ†Â°ng backend gÃ¡Â»Âi helper `iptables-*` thay vÃƒÂ¬ `ufw-*`.
  - TÃ¡ÂºÂ¡o chain idempotent `opanel_ent_INPUT`, `opanel_ent_USER`, `opanel_ent_BLOCKLIST`; khÃƒÂ´ng flush rule ngoÃƒÂ i opanel-ent.
  - `Enable` tÃ¡ÂºÂ¡o default allow SSH/panel/web/mail, established/loopback; `Disable` chÃ¡Â»â€° thÃƒÂ¡o chain opanel-ent vÃƒÂ  set INPUT ACCEPT.
  - Rule user lÃ†Â°u declarative Ã¡Â»Å¸ `/var/lib/opanel-ent/firewall/rules.json`; UI delete theo sÃ¡Â»â€˜ thÃ¡Â»Â© tÃ¡Â»Â± managed rule, khÃƒÂ´ng theo UFW number.

- URL blocklist:
  - Timer `opanel-ent-firewall-blocklist.timer` fetch TXT hÃ¡ÂºÂ±ng ngÃƒÂ y 01:00.
  - Parse IPv4/IPv6 bÃ¡ÂºÂ±ng Python `ipaddress`, dedupe, ghi work file.
  - DÃƒÂ¹ng staged sets `opanel_ent_blocklist4_new`, `opanel_ent_blocklist6_new`, `ipset swap` sang active sets rÃ¡Â»â€œi apply `iptables -m set --match-set ... src -j DROP`.
  - KhÃƒÂ´ng reload webserver khi blocklist Ã„â€˜Ã¡Â»â€¢i.

## Interfaces
- Public API giÃ¡Â»Â¯ tÃ†Â°Ã†Â¡ng thÃƒÂ­ch: `/firewall/allow-port`, `/firewall/allow-ip`, `/firewall/block-ip`, `/firewall/blocklists`.
- Response firewall status thÃƒÂªm `engine=iptables` vÃƒÂ  blocklist status thÃƒÂªm `engine=iptables+ipset`.
- Website schema v1 giÃ¡Â»Â¯ DB fields `nginx_custom`, `nginx_config_mode`, `nginx_rewrite_mode` Ã„â€˜Ã¡Â»Æ’ migration an toÃƒÂ n; thÃƒÂªm alias public `webserver_custom`, `webserver_config_mode`, `webserver_rewrite_mode`.
- Route/UI cÃ…Â© `nginx-custom` giÃ¡Â»Â¯ deprecated alias; frontend dÃƒÂ¹ng nhÃƒÂ£n OpenLiteSpeed/Webserver.

## Test Plan
- Unit test OLS renderer: WordPress/PHP/static, SSL, aliases, redirects, rewrite modes, custom include, WAF, HTTP flood, LSCache.
- Unit test firewall renderer/parser: enable/disable/reload, protected defaults, user allow/deny, IPv4/IPv6, delete managed rule.
- Unit test blocklist: TXT parsing, invalid rows ignored, IPv4/IPv6 split, staged `ipset swap`, rollback on failure.
- Migration test trÃƒÂªn Ubuntu 24.04 VM: opanel-ent hiÃ¡Â»â€¡n cÃƒÂ³ -> opanel-ent, regenerate all vhosts, OLS config test pass, panel login, website PHP, SSL renew dry run, firewall persistence after reboot.
- Static checks: khÃƒÂ´ng cÃƒÂ²n runtime dependency vÃƒÂ o `ufw`/`nginx` ngoÃƒÂ i deprecated aliases, migration notes, vÃƒÂ  archived legacy snippets.

## Assumptions
- Default chÃ¡Â»Ân: Ã„â€˜Ã¡Â»â€¢i tÃƒÂªn toÃƒÂ n bÃ¡Â»â„¢ sang `opanel-ent` nhÃ†Â°ng giÃ¡Â»Â¯ alias migration tÃ¡Â»Â« `opanel-ent`.
- Default chÃ¡Â»Ân: dÃƒÂ¹ng `ipset + iptables`; `iptables` vÃ¡ÂºÂ«n lÃƒÂ  enforcement layer chÃƒÂ­nh, `ipset` chÃ¡Â»â€° chÃ¡Â»Â©a danh sÃƒÂ¡ch lÃ¡Â»â€ºn Ã„â€˜Ã¡Â»Æ’ trÃƒÂ¡nh hÃƒÂ ng nghÃƒÂ¬n rule.
- Default chÃ¡Â»Ân: dÃƒÂ¹ng LSPHP/LSAPI native thay vÃƒÂ¬ PHP-FPM.
- Custom Nginx snippets cÃ…Â© khÃƒÂ´ng auto-compatible; migration sÃ¡ÂºÂ½ archive toÃƒÂ n bÃ¡Â»â„¢, chÃ¡Â»â€° auto-enable phÃ¡ÂºÂ§n chuyÃ¡Â»Æ’n Ã„â€˜Ã¡Â»â€¢i an toÃƒÂ n.
- TÃƒÂ i liÃ¡Â»â€¡u nÃ¡Â»Ân: OpenLiteSpeed repo/LSPHP install, include files, ModSecurity, vÃƒÂ  ipset/iptables behavior. Sources: OpenLiteSpeed install docs ([docs.openlitespeed.org](https://docs.openlitespeed.org/installation/repo/)), PHP/LSPHP docs ([docs.openlitespeed.org](https://docs.openlitespeed.org/config/php/)), OLS include docs ([docs.openlitespeed.org](https://docs.openlitespeed.org/config/advanced/includes/)), OLS ModSecurity docs ([docs.openlitespeed.org](https://docs.openlitespeed.org/modules/modsecurity/)), ipset/iptables man pages ([ipset.netfilter.org](https://ipset.netfilter.org/ipset.man.html?utm_source=openai)) ([man7.org](https://man7.org/linux/man-pages/man8/iptables-restore.8.html?utm_source=openai)).
