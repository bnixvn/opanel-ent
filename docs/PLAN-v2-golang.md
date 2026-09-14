# OPanel Enterprise v2 — Viết lại bằng Go

> Thay thế `PLAN.md`, `upgrade_plan.md` (phần kiến trúc) và plan port CloudLinux trước đó.
> Codebase Python/FastAPI hiện tại chuyển vai trò thành **đặc tả tham chiếu**, không còn là sản phẩm.

---

## 1. Context

### Vì sao viết lại

OPanel Enterprise hiện tại: ~26.700 dòng Python/FastAPI + ~8.100 dòng bash + ~5.500 dòng React, chạy trên Ubuntu 24.04 + OpenLiteSpeed + iptables.

Mục tiêu kinh doanh là bán shared hosting trên nền CloudLinux + LiteSpeed Enterprise. Việc port Python sang EL10 là khả thi (~250 điểm sửa) nhưng quyết định chuyển sang Go dựa trên bốn lý do, trong đó lý do thứ hai là quan trọng nhất về dài hạn:

1. **Hiệu năng và tài nguyên.** FastAPI + uvicorn chiếm ~150–200MB RSS ở trạng thái nghỉ. Một binary Go tương đương chiếm ~15–25MB. Trên server bán shared hosting, RAM panel ăn là RAM không bán được.

2. **Không xung đột Python với CloudLinux.** Toàn bộ toolchain CloudLinux (`cldetect`, `cagefsctl`, `cloudlinux-selector`, `lvectl`, `mysqlgovernor.py`) là Python bám vào system python của EL. Panel dựng thêm venv trên cùng máy là rủi ro dài hạn mỗi lần CloudLinux cập nhật. Static binary Go loại bỏ hẳn lớp rủi ro này.

3. **Triển khai và cập nhật.** `CGO_ENABLED=0 go build` cho một file duy nhất, nhúng cả frontend. Update = thay file + restart, rollback = đổi lại file. Không pip, không venv, không build frontend trên server production.

4. **Bỏ được hai dependency ngoài.** ACME làm in-process bằng `lego` → **không cần certbot** (và không cần `python3-certbot-dns-cloudflare` từ EPEL). Nén/giải nén tar+zstd in-process → không shell ra `tar`/`zstd`, backup stream thẳng không cần file tạm.

### Cái gì được tái sử dụng

Không tái sử dụng code, nhưng tái sử dụng rất nhiều **tri thức** — đây là lý do rewrite này rẻ hơn greenfield đáng kể:

| Nguồn | Dùng làm |
|---|---|
| `backend/app/models/entities.py`, `backend/alembic/versions/` | Đặc tả schema DB |
| `backend/app/templates/openlitespeed/*.j2` | Template vhost OLS — chỉ đổi cú pháp Jinja2 → `text/template` |
| `backend/app/services/mariadb.py:222-260` `_TIERS`, `php.py:221-234` `_PHP_TIERS` | Bảng số auto-tune, port thẳng |
| `backend/app/services/waf.py` | Bộ rule ModSecurity WordPress/Laravel/PHP |
| `backend/app/services/da_import.py` (1.375 dòng) | Đặc tả format archive DirectAdmin |
| `docs/whmcs-opanel-ent-contract.md`, `modules/servers/opanelent/` | Contract provisioning — giữ nguyên, module PHP không đổi |
| `frontend/src/` (5.485 dòng React) | **Giữ lại**, tách module + nhúng vào binary |

### Lộ trình hai tầng (theo yêu cầu)

**Stage A — AlmaLinux 10, hoàn toàn miễn phí, vận hành được thật.**
AlmaLinux 10 + OpenLiteSpeed + multi-PHP lsphp + MariaDB 11.8 LTS + phần còn lại. Không phụ thuộc license nào. Đây là sản phẩm bán được.

**Stage B — chồng thêm CloudLinux + LiteSpeed Enterprise.**
`cldeploy` → CloudLinux 10, cài LSWS Ent, chuyển alt-php, bật LVE. **Nếu LSWS văng license thì tự động rơi về OpenLiteSpeed** và site vẫn chạy.

Yêu cầu switch này quyết định kiến trúc: webserver và PHP phải là abstraction **runtime**, không phải lựa chọn lúc build, và **mọi trạng thái site phải nằm trong DB** để render lại từ đầu sang format khác bất cứ lúc nào.

---

## 2. Stack

| Lớp | Chọn | Lý do |
|---|---|---|
| Ngôn ngữ | Go (bản stable hiện tại, tối thiểu 1.24) | 1.24 có `os.Root` — bắt buộc cho file manager, xem §4.7 |
| HTTP | `net/http` + `go-chi/chi/v5` | Router mỏng, tương thích stdlib, không lock framework |
| DB | SQLite qua `modernc.org/sqlite` | **Pure Go, không cgo** — giữ được static binary. Không dùng `mattn/go-sqlite3` |
| Query | `sqlc` | Sinh Go type-safe từ SQL. Không ORM |
| Migration | `pressly/goose`, nhúng bằng `embed.FS` | |
| Template config | `text/template` (stdlib) | Thay Jinja2, nhúng bằng `embed.FS` |
| ACME/SSL | `go-acme/lego/v4` | In-process, HTTP-01 + DNS-01 Cloudflare. **Bỏ certbot** |
| Nén | `klauspost/compress/zstd` + `archive/tar` | Stream, không file tạm |
| WebSocket | `coder/websocket` | Terminal + log tail |
| PTY | `creack/pty` | Terminal |
| TOTP | `pquerna/otp` | 2FA |
| Password | `golang.org/x/crypto/argon2` | Thay bcrypt/passlib |
| Log | `log/slog` (stdlib) | JSON structured, ra journald |
| Lint | `golangci-lint` | |
| Frontend | React + Vite (giữ), build → `embed.FS` | |

Nguyên tắc: **ưu tiên stdlib**. Mỗi dependency ngoài phải trả lời được "nếu tự viết thì tốn bao nhiêu".

---

## 3. Kiến trúc

### 3.1 Ba binary

```
opanel-api     chạy user opanel     HTTP API + frontend nhúng, không có quyền root
opanel-agent   chạy root            unix socket, thực thi thao tác đặc quyền
opanelctl      CLI                  install, update, switch webserver, diagnose, recovery
```

Đây là thay đổi kiến trúc lớn nhất so với v1. Hiện tại v1 dùng sudoers + `opanel-ent-helper.sh` 4.565 dòng bash với allowlist — mọi thao tác đặc quyền là một chuỗi shell được ghép từ input người dùng, phải chống bằng regex (`DANGEROUS_DIRECTIVES_RE`, `validate_php_config_file`, `require_php_version`...).

Thiết kế mới:

- `opanel-agent` lắng nghe `/run/opanel/agent.sock`, mode `0660 root:opanel`.
- Xác thực bằng `SO_PEERCRED`: chỉ chấp nhận uid của user `opanel`.
- Giao thức: JSON request `{action, version, payload}` trên unix socket, mỗi action là một struct Go có kiểu.
- **Không bao giờ ghép chuỗi shell.** Mọi lệnh ngoài chạy qua `exec.Command(bin, args...)` với argv slice. Điều này xoá sạch một lớp bug: shell injection, quoting, IFS, glob ngoài ý muốn.
- Mọi action ghi audit: caller uid, action, payload đã redact, kết quả, thời gian.
- Agent có timeout per-action và giới hạn concurrency.

```go
// internal/agent/action.go
type Action[In, Out any] struct {
    Name    string
    Version int
    Handler func(ctx context.Context, in In) (Out, error)
}

// Ví dụ — thay cho helper action "php-config-write"
type PHPConfigWriteIn struct {
    Version string            `json:"version" validate:"phpver"`
    Values  map[string]string `json:"values"`
}
```

`opanelctl` nói chuyện trực tiếp với agent (chạy bằng root), dùng khi panel chết hoặc để recovery.

### 3.2 Layout repo

```
cmd/
  opanel-api/          main
  opanel-agent/        main
  opanelctl/           main
internal/
  config/              env + file config, validate lúc khởi động
  db/                  sqlc generated, migrations (embed)
  auth/                session, argon2, TOTP, RBAC, API token, SSO token
  httpapi/             router, middleware, handler, DTO
    middleware/        auth, rbac, ratelimit, audit, recover
  agentclient/         client gọi agent
  agent/
    actions/           mỗi file một nhóm action
  platform/
    distro/            detect: almalinux10 | cloudlinux10 (dùng cldetect, KHÔNG dùng os-release)
    pkg/               dnf wrapper (install/remove/query/check-update)
    svc/               systemd (start/stop/enable/status/journal)
    selinux/           getenforce, restorecon, semanage
    user/              useradd/usermod/groupadd, /etc/shadow
  webserver/           interface + switch + failover watchdog
    ols/               renderer .conf
    lsws/              renderer .xml
    tmpl/              embed.FS template cả hai
  phpmgr/              interface
    lsphp/             LiteSpeed repo RPM
    altphp/            CloudLinux alt-php
  mariadb/             provision DB/user, tuner, phpMyAdmin SSO
  sites/               vòng đời website
  panelusers/          panel user ↔ linux user ↔ SFTP
  ssl/                 lego, lưu trữ cert, auto-renew
  firewall/            nftables renderer + blocklist
  waf/                 ModSecurity rule per-site
  backup/              tar+zstd stream, scheduler, SFTP target, restore
  files/               file manager (os.Root)
  cron/                cron job manager
  malware/             ClamAV + LMD
  metrics/             CPU/RAM/disk/net, LVE usage
  terminal/            pty over websocket
  provisioning/        API cho WHMCS
  daimport/            DirectAdmin importer
  cloudlinux/          lve, cagefs, selector, governor (Stage B)
  wordpress/           WP installer + WP-CLI
web/                   React source
migrations/            .sql
installer/             bootstrap shell (tối thiểu) + opanelctl làm phần còn lại
docs/
```

### 3.3 Installer

Chỉ một script shell mỏng: cài `curl`, tải binary `opanelctl` đúng arch, chạy `opanelctl install`. Toàn bộ logic cài đặt nằm trong Go — có kiểu, test được, không phải 1.332 dòng bash.

```bash
bash <(curl -fsSL https://get.opanel.io/install.sh)
```

`opanelctl install` chạy các step idempotent, mỗi step có `Check()` và `Apply()`, in tiến trình, và **resume được** nếu đứt giữa chừng.

---

## 4. Thiết kế các phần chính

### 4.1 Webserver abstraction (lõi của yêu cầu switch)

```go
// internal/webserver/backend.go
type Backend interface {
    Name() string                                  // "ols" | "lsws"
    Installed() bool
    Healthy(ctx context.Context) error             // process + port + license
    Render(s Site) ([]byte, error)                 // pure, test bằng golden file
    Write(ctx context.Context, s Site) error
    Remove(ctx context.Context, domain string) error
    Suspend(ctx context.Context, domain string) error
    Restore(ctx context.Context, domain string) error
    TestConfig(ctx context.Context, dir string) error
    Reload(ctx context.Context) error
    ReadLog(domain, kind string, lines int) (string, error)
    SetWAF(ctx context.Context, domain string, on bool) error
}
```

`Site` là struct chuẩn hoá sinh từ DB — document root, app type, PHP version, aliases, redirects, rewrite mode, SSL paths, WAF flag, custom directives, linux user. **Không có thông tin nào chỉ tồn tại trong file config.** Đây là điều kiện tiên quyết để switch backend an toàn.

`Render` là hàm thuần (không I/O) → test bằng golden file cho cả hai backend, đối chiếu ma trận: wordpress/php/static × có/không SSL × 5 rewrite mode × có/không alias × có/không WAF.

**Khác biệt hai backend:**

| | OpenLiteSpeed | LiteSpeed Enterprise |
|---|---|---|
| Format | `.conf` plain text | **XML** |
| Config chính | `/usr/local/lsws/conf/httpd_config.conf` | `/usr/local/lsws/conf/httpd_config.xml` |
| Vhost | `conf/vhosts/<d>/vhconf.conf` | `conf/vhosts/<d>/vhconf.xml` |
| ModSecurity | package `ols-modsecurity` | built-in |
| extApp PHP | một extApp mỗi site | suEXEC sẵn → extApp ở server level, một cái mỗi PHP version |
| License | không | có, và có thể hết hạn |

Vì hai backend không thể cùng nghe 80/443, switch có downtime vài giây — chấp nhận và ghi rõ trong docs.

**⚠ `TestConfig()` cho OLS — đo thực tế ở Phase 0, đừng viết theo trực giác:**

| Trường hợp | exit | `[ERROR]` |
|---|---|---|
| Config hợp lệ | **1** | 0 |
| Directive sai tên | 1 | 0 |
| `configFile` trỏ file không tồn tại | **2** | 2 |
| Listener trùng port | 1 | 0 |

```go
// exit==1 LÀ THÀNH CÔNG. Viết `rc == 0` sẽ coi mọi config đúng là hỏng.
ok := rc != 2 && !bytes.Contains(out, []byte("[ERROR]"))
```

Quan trọng hơn: **OLS bỏ qua directive sai tên, chỉ cảnh báo** — một lỗi chính tả trong renderer sẽ lặng lẽ không có tác dụng và `-t` không bắt được. Nên **golden-file test của `Render()` mới là lưới an toàn thật sự**, `TestConfig()` chỉ bắt được lỗi cấu trúc. Điều này nâng ưu tiên bộ test renderer lên mức bắt buộc, không phải "nice to have".

Chi tiết vận hành khác từ Phase 0: thư mục `conf/` thuộc `lsadm:nobody` mode 0750 → **ghi xong phải `chown lsadm:nobody`**; service là `lshttpd.service` (`lsws`/`openlitespeed` là alias); `lswsctrl restart` là graceful zero-downtime; OLS mặc định nghe 8088/7080 nên bước cài phải đổi sang 80/443.

**`opanelctl webserver switch <ols|lsws>`:**

1. Kiểm tra backend đích `Installed()` và (với lsws) license hợp lệ
2. Render toàn bộ site ra `/var/lib/opanel/webserver/staging/<backend>/`
3. `TestConfig()` trên staging
4. Snapshot config đang chạy sang `/var/lib/opanel/webserver/last-good/<backend>/`
5. Stop backend hiện tại
6. Move staging → live (atomic rename từng thư mục)
7. Start backend đích
8. Health check: HTTP tới panel + mẫu ngẫu nhiên tối đa 10 domain, timeout 30s
9. Thất bại bất kỳ bước nào từ 5 → rollback đầy đủ về backend cũ, giữ nguyên trạng thái DB, ghi audit + alert

**Auto-failover khi LSWS mất license** (yêu cầu cụ thể):

Watchdog goroutine trong `opanel-agent`, chu kỳ 60s:

- Đọc hạn license (`lshttpd -V`, file `serial.no`/`trial.key`), cảnh báo trước 14 ngày
- Grep error log LSWS tìm pattern license (`license`, `expired`, `exceeded`, `invalid serial`)
- Kiểm tra process sống + port 80/443 accept connection

Khi phát hiện degraded:
1. Thử restart LSWS một lần, chờ 20s
2. Vẫn fail → **tự động switch sang OLS** theo đúng quy trình trên
3. Gửi alert (email + webhook), ghi audit, hiện banner đỏ trong panel với lý do cụ thể
4. **Không tự switch ngược lại** — tránh flapping. Sau khi gia hạn, admin bấm nút switch thủ công.

Cấu hình: `webserver.auto_failover = true|false`, `webserver.preferred = lsws|ols`. Trạng thái hiện tại luôn đọc được qua `GET /api/webserver/status`.

### 4.2 PHP abstraction

```go
// internal/phpmgr/provider.go
type Provider interface {
    Name() string                        // "lsphp" | "altphp"
    Available() bool
    Installed(ctx context.Context) ([]Version, error)
    Installable(ctx context.Context) ([]Version, error)
    Install(ctx context.Context, v string) error
    Uninstall(ctx context.Context, v string) error   // v1 Python thiếu hàm này
    LSAPIBinary(v string) string         // cho vhost
    CLIBinary(v string) string           // cho WP-CLI, cron, terminal
    IniDropIn(v string) string           // nơi ghi 99-opanel.ini
    Tune(ctx context.Context, v string, c TuneConfig) error
}
```

| | Stage A: `lsphp` | Stage B: `altphp` |
|---|---|---|
| Nguồn | LiteSpeed repo el10 ✅ đã xác minh | CloudLinux, `dnf groupinstall alt-php` |
| **Version có sẵn** | **8.1, 8.2, 8.3, 8.4, 8.5** — không có 7.4 | 5.6 → 8.4 |
| LSAPI binary | `/usr/local/lsws/lsphpNN/bin/lsphp` | `/opt/alt/phpNN/usr/bin/lsphp` |
| CLI binary | `/usr/local/lsws/lsphpNN/bin/php` | `/opt/alt/phpNN/usr/bin/php` |
| ini drop-in | `/usr/local/lsws/lsphpNN/etc/php.d/99-opanel.ini` ✅ | chốt khi convert |
| ionCube | ✅ có RPM `lsphpNN-ioncube` | có sẵn |
| HardenedPHP (vá bảo mật PHP EOL) | không | **có** |

Cài `openlitespeed` tự kéo theo `lsphp83` làm dependency — provider phải coi đó là chuyện bình thường, không phải version do người dùng chọn.

Chọn provider tự động: CloudLinux + có alt-php → `altphp`, ngược lại `lsphp`. Override được bằng config.

**Quy tắc bất biến:** DB lưu `"8.3"`, không bao giờ lưu đường dẫn. Đổi provider = regenerate vhost, không migrate DB. Đây chính là chỗ v1 làm sai — `_php_version_from_config` phải regex ngược đường dẫn ra khỏi file config.

**Một nguồn version duy nhất.** v1 có sáu danh sách rời rạc và đã lệch nhau. v2: một biến `phpmgr.SupportedVersions`, backend/frontend/validator đều derive từ đó, frontend lấy qua `GET /api/php/versions`.

**Bỏ pool tuner tự chế.** v1 có ~430 dòng bash tính `LSAPI_CHILDREN` per-site (`helper.sh:2783-3213`). Ở Stage B, LVE đã cap PMEM/NPROC theo user nên hai cơ chế chọi nhau. v2 chỉ giữ auto-tune OPcache + memory_limit; số worker để webserver và LVE lo.

**Sửa bug sẵn có:** v1 chạy WP-CLI và cron bằng PHP mặc định của hệ thống chứ không phải PHP của site (`helper.sh:4441,4448`). v2 dùng `CLIBinary(site.PHPVersion)`.

### 4.3 Platform layer

`distro.Detect()` trả về `{Family, Version, IsCloudLinux, CLEdition}`.

**Quan trọng:** sau khi `cldeploy` convert, `/etc/os-release` **vẫn báo AlmaLinux**. Phát hiện CloudLinux bắt buộc qua `cldetect --detect-edition`, không đọc os-release.

`pkg` bọc dnf: `Install`, `Remove`, `Installed`, `CheckUpdate`, `SecurityUpdates`. API `/updates/*` giữ nguyên contract của v1; `mode=security|all` map sang `dnf --security` và `dnf-automatic` `upgrade_type`.

`svc` bọc systemd qua D-Bus (`godbus`) thay vì gọi `systemctl` — nhanh hơn và có kiểu. Fallback `systemctl` nếu D-Bus không sẵn.

**SELinux:** Phase 0 cho thấy image AlmaLinux của provider **tắt sẵn SELinux ở mức boot** (`SELINUX=disabled` trong `/etc/selinux/config`), và `semanage`/`audit2allow` không được cài. Bật lại không chỉ là đổi config — cần `SELINUX=permissive` + `touch /.autorelabel` + **reboot** để relabel toàn hệ thống.

Quyết định Stage A: **chấp nhận SELinux tắt** (đúng thực tế đa số server hosting), nhưng installer phải **phát hiện và ghi trạng thái vào báo cáo cài đặt**, và code tuyệt đối không được ngầm dựa vào việc SELinux đang tắt. Bật + đóng gói policy module `opanel.pp` đưa vào backlog sau Stage B.

**Disk quota:** `/` là xfs mount `noquota`, không có phân vùng `/home` riêng. XFS không bật quota bằng remount — root fs cần `rootflags=uquota` trong kernel cmdline + reboot. Stage A dùng quota mềm ở tầng ứng dụng (như v1) và ghi rõ đó là quota mềm.

**⚠ sshd — chỗ rất dễ làm sai:** `/etc/ssh/sshd_config` có `Include /etc/ssh/sshd_config.d/*.conf` ở **dòng 15, trên đầu file**. Hệ quả ngược nhau cho hai loại cấu hình:

- **Directive toàn cục** (ví dụ `PasswordAuthentication`): phải đặt trong `sshd_config.d/` với prefix sắp trước (`10-opanel.conf`), vì sshd lấy giá trị **đầu tiên** gặp được.
- **`Match` block** (SFTP chroot): **không được** đặt trong `sshd_config.d/`. `Match` có hiệu lực tới `Match` kế tiếp hoặc hết file — include ở dòng 15 nghĩa là toàn bộ phần còn lại của file chính rơi vào trong block đó. → **append vào cuối `/etc/ssh/sshd_config`.**

Đổi `Subsystem sftp` từ `/usr/libexec/openssh/sftp-server` sang `internal-sftp` để chroot hoạt động.

**User:** máy không có `www-data`, `apache`, `nginx` — chỉ có `nobody` (65534). Tạo `opanel-web` như thiết kế. `UID_MIN=1000`. OLS tự tạo user `lsadm` (994).

### 4.4 Firewall — nftables

`iptables` và `ipset` đã deprecated từ RHEL 9. v2 dùng nftables native ngay từ Stage A.

- Một table `inet opanel`, chain `input` hook filter
- Named set `blocklist4`/`blocklist6` với `flags interval` — thay hẳn ipset
- Rule declarative trong DB (không phải file JSON như v1)
- Render toàn bộ ruleset ra file rồi `nft -f` — atomic, thay cả table trong một transaction
- **Không parse text output.** v1 parse `iptables -L --line-numbers` (`helper.sh:509-519`) — nguồn bug lớn nhất của module đó. v2 dùng `nft -j` (JSON) khi cần đọc lại
- Persist qua `/etc/sysconfig/nftables.conf`, `systemctl enable nftables`
- Bảo vệ mặc định: SSH (phát hiện port thật từ sshd config), panel port, 80/443, mail, loopback, established/related. Không cho tự khoá mình ra ngoài
- `firewalld` bị disable + mask lúc cài

### 4.5 SSL — bỏ certbot

`go-acme/lego/v4` in-process:

- HTTP-01 qua webroot chung `/var/www/opanel-acme`
- DNS-01 Cloudflare (giữ tính năng v1 đang có)
- Lưu cert vào `/var/lib/opanel/ssl/<domain>/`, quyền chặt, symlink sang chỗ webserver đọc
- Renew bằng goroutine ticker trong `opanel-api`, kiểm tra mỗi 12h, renew khi còn <30 ngày
- Sau renew: reload webserver + reload panel TLS **không cần restart** (`tls.Config.GetCertificate` đọc từ store nóng)

Lợi ích: bỏ certbot, bỏ EPEL dependency, bỏ deploy-hook shell, bỏ cả việc mirror cert khỏi `/etc/letsencrypt` (v1 phải làm vì thư mục đó là 0700).

### 4.6 MariaDB

- Stage A: MariaDB **11.8 LTS** từ repo chính thức (hỗ trợ tới 6/2028). AppStream của AlmaLinux 10 chỉ có 10.11.18 nên bắt buộc thêm repo:
  `curl -LsS https://r.mariadb.com/downloads/mariadb_repo_setup | bash -s -- --os-type=rhel --os-version=10 --mariadb-server-version=mariadb-11.8`
- ⚠ Package tên **`MariaDB-server` / `MariaDB-client` (viết hoa)** — khác `mariadb-server` của AppStream. Cài nhầm sẽ ra 10.11.
- Driver `go-sql-driver/mysql`, kết nối qua unix socket **`/var/lib/mysql/mysql.sock`** (không phải `/run/mysqld/mysqld.sock` kiểu Debian)
- Tuner: port thẳng bảng tier từ `mariadb.py:222-260`, ghi `/etc/my.cnf.d/99-opanel.cnf`. **Chỉ một tuner** — v1 có hai bản trùng nhau ở bash và Python
- phpMyAdmin: EL không có package → vendor tarball vào `/usr/share/phpmyadmin`, checksum verify, tự sinh `config.inc.php`. SSO giữ cơ chế token 60s của v1
- **Ghi chú Stage B:** nếu sau này bật MySQL Governor thì phải thay MariaDB bằng bản CloudLinux build qua `mysqlgovernor.py`. Đây là migration có downtime — thiết kế lớp `mariadb` để không giả định nguồn package

### 4.7 File manager — đã làm (S5)

Dùng `os.Root` (Go 1.24+) — mở một root handle theo home của user rồi mọi thao tác đi qua handle đó. Kernel đảm bảo không thoát ra ngoài, kể cả qua symlink hay `..`. Thay thế toàn bộ lớp kiểm tra path thủ công của v1 bằng bảo đảm ở tầng syscall.

Đã kiểm chứng trên host thật: symlink trỏ ra `/etc` và `/etc/shadow` đều bị từ chối ở cả đọc lẫn ghi (`path escapes from parent`), tên file upload dạng `../../../tmp/x` bị rút về basename.

Hiện trạng:

- Agent: `fs.list`, `fs.read`, `fs.write`, `fs.mkdir`, `fs.delete`, `fs.rename`, `fs.chmod`, `fs.install`, `fs.stage`
- API: `/api/files*`, quyền do `filemanager.ResolveOwner` quyết — end user chỉ với tới home của chính mình, staff phải nêu `?owner=`
- Upload/download đi qua file staging ở `/var/lib/opanel/uploads`: agent chạy root nên đọc/ghi được nhà khách, API chạy `opanel` thì không. Upload stream thẳng từ multipart (không `ParseMultipartForm`), trần 256 MB/file; editor trần 1 MB
- chmod chỉ nhận 12 bit permission — setuid/setgid/sticky bị từ chối ở tầng validate
- UI: tab Files với breadcrumb, editor, kéo-thả upload, đổi tên tại chỗ

Còn thiếu: nén/giải nén in-process, thao tác đệ quy có worker pool, sao chép/di chuyển giữa các thư mục.

### 4.8 Backup — đã làm (S6)

Đơn vị backup là **tài khoản**, không phải site: restore site mà thiếu database thì site vẫn hỏng, và hai thứ đó chỉ nối với nhau qua tài khoản.

Hiện trạng:

- `archive/tar` + `compress/gzip` stdlib. **Không dùng zstd** như dự định ban đầu: đổi lấy việc mở được bằng `tar` trên bất kỳ máy nào, kể cả máy không chạy panel này. Backup là thứ người ta mở ra vào ngày tồi tệ
- Layout archive: `manifest.json` (member đầu tiên) → `databases/<name>.sql` → `files/...`. Manifest nằm đầu để `backup.inspect` chỉ phải giải nén một member thay vì cả archive
- Manifest ghi: format version, owner, home, danh sách site (domain, alias, app_type, php_version, document_root, rewrite_mode), danh sách database. **Không ghi số file** — số đó chỉ biết sau khi đi hết cây thư mục, ghi vào manifest thì phải đẩy manifest xuống cuối. Số file nằm ở row `backups` trong SQLite
- Dump SQL qua `mariadb-dump --single-transaction --add-drop-database`. `--add-drop-database` khiến restore là **thay thế thật**, không phải merge: bảng tạo sau khi backup sẽ biến mất. Kèm theo đó là mất grant, nên service tự cấp lại grant từ bảng `db_grants` sau khi restore xong
- Symlink lưu nguyên là symlink, không đi theo: link của khách trỏ tới `/etc` không được kéo cấu hình host vào archive của khách
- Restore giải nén qua `os.Root` trên home của chủ sở hữu — zip-slip bị kernel chặn, không phải bị hàm kiểm tra path chặn. Archive còn ghi tên chủ sở hữu và restore từ chối nếu không khớp
- Archive nằm ở `/var/backups/opanel/<owner>/`, root-only. **Không** nằm dưới `/var/lib/opanel`: đó là `StateDirectory` của service API, và systemd chown cả cây đó cho user `opanel` mỗi lần service khởi động
- Chạy nền: row `backups` ghi trước khi bắt đầu, goroutine gọi agent với budget 2 giờ (`agent.RegisterSlow`), UI poll 5 giây một lần. Row kẹt ở `running` sau khi panel restart được đánh dấu `failed` lúc khởi động
- Scheduler: goroutine ticker 10 phút. Lịch là "hằng ngày/hằng tuần lúc mấy giờ", không phải cron expression. `Due()` viết theo kiểu "đã tới giờ và chưa chạy trong kỳ này" nên server tắt lúc 3h sáng bật lại lúc 9h vẫn chạy backup, thay vì bỏ qua cả ngày
- Retention: chỉ xoá backup `scheduled`, không đụng backup người ta tự bấm trước khi làm việc nguy hiểm. Xoá sau khi backup mới đã xong, không xoá trước

Còn thiếu: SFTP target (`pkg/sftp`), restore sang server khác (hiện phải tạo sẵn tài khoản cùng tên), restore chọn từng site.

### 4.9 Frontend

Giữ React + Vite. Ba việc:

1. **Tách `App.jsx`** (5.485 dòng) thành module theo trang: dashboard, sites, databases, files, backups, firewall, waf, php, users, plans, updates, settings, terminal
2. Chuyển sang TypeScript dần (không bắt buộc trước Stage A)
3. `go:embed web/dist` → frontend nằm trong binary, serve bằng `http.FileServerFS`

Xoá các chuỗi apt còn sót: `App.jsx:23` `DEFAULT_SERVICE_NAMES`, `:2487`, `:2726`, `:4325`, `:4370`.

Thêm mới: banner trạng thái webserver (đang chạy backend nào, license còn bao lâu, có đang ở chế độ failover không), và LVE usage ở Stage B.

### 4.10 Xác thực và phân quyền

- Session server-side trong SQLite + cookie `HttpOnly; Secure; SameSite=Lax`. Không JWT — cần revoke được ngay khi suspend user
- Argon2id cho mật khẩu
- TOTP 2FA (`pquerna/otp`), recovery code
- RBAC: admin / reseller / end_user, kiểm tra ở middleware theo scope
- API token cho WHMCS với scope `provisioning:manage`, `sso:create` — giữ đúng contract v1
- SSO token dùng một lần, hạn 60s
- Rate limit theo IP cho login và API token

---

## 5. Lộ trình

Mỗi milestone phải **chạy được thật**, không phải nửa vời. Ước lượng là công sức cho một người làm full-time.

### ~~Phase 0 — Spike trên VPS AlmaLinux 10~~ ✅ XONG 2026-09-14

Kết quả đầy đủ: **`docs/bringup-el10.md`**. Script: `installer/phase0-recon.sh`.
Máy: AlmaLinux 10.1, KVM, 2 vCPU / 1.9 GiB / 20 GB xfs.

Bảy câu hỏi đều có đáp án. Bốn điều buộc phải sửa so với bản plan đầu:

1. **lsphp trên el10 chỉ có 8.1–8.5, KHÔNG có 7.4** → xem §Quyết định sản phẩm bên dưới
2. **`openlitespeed -t` trả `exit 1` khi config ĐÚNG, `exit 2` khi sai** → §4.1
3. **dnf 4.20.0, không phải dnf5** → giả định về cú pháp `dnf group install` bỏ đi
4. **SELinux bị tắt sẵn** trong image của provider → §4.3

Xác nhận đúng như dự kiến: `virt=kvm` (Stage B khả thi), ini scan dir `/usr/local/lsws/lsphp{NN}/etc/php.d/`, `MariaDB-server 11.8.9` chạy tốt, `nft -j` hoạt động, máy không có sẵn firewalld/iptables, Go 1.26.7 và Node 22 có sẵn trong repo AlmaLinux, ionCube cài được bằng RPM.

#### ⚠ Quyết định sản phẩm phát sinh: PHP 7.4

Repo LiteSpeed cho el10 không có `lsphp74`. Nghĩa là **Stage A chỉ phục vụ được khách dùng PHP ≥ 8.1.** Khách còn chạy WordPress/plugin cũ hoặc Laravel cũ trên 7.4 sẽ không dùng được.

Đường ra duy nhất là **Stage B**: alt-php của CloudLinux có 5.6 → 8.4 kèm HardenedPHP (vá bảo mật cho bản đã EOL). Đây là lý do làm Stage B mạnh hơn cả LVE.

**✅ ĐÃ CHỐT (2026-09-14): chấp nhận Stage A giới hạn PHP 8.1+.** Khách cần 7.4 trở xuống phục vụ ở Stage B. `phpmgr` vẫn phải thiết kế đủ tổng quát để thêm provider mà không đụng tầng trên.

#### Việc chặn Phase 2 — cloud firewall của provider

Provider chỉ mở cổng 22. Đo được: 80/443 trả `filtered` kể cả khi nftables đã cho qua và không có service nào nghe (lẽ ra phải `refused`). **Phải mở 80, 443 và cổng panel trong panel nhà cung cấp**, nếu không thì không test được website nào và không cấp được SSL qua HTTP-01. Chi tiết ở `docs/bringup-el10.md` §14b.

### ~~Phase 1 — Bộ khung~~ ✅ XONG 2026-09-14

4.169 dòng code + 1.194 dòng test. Ba binary static (`CGO_ENABLED=0`), chạy thật trên VPS AlmaLinux 10.2.

**Cổng ra đã đạt:** login vào API rỗng chạy được, agent nhận và ghi log action; thêm phần chưa yêu cầu — restart service thật qua panel, và bài kiểm tra ranh giới SO_PEERCRED.

Đã có: config có validate; schema DB + goose nhúng; auth đầy đủ (argon2id, session hash-only, TOTP + recovery code dùng một lần, RBAC, API token có scope); agent + unix socket + SO_PEERCRED + allowlist unit; platform layer (distro/pkg/svc/run); `opanelctl` (doctor, db, agent, service, user); systemd unit đã hardening; Makefile + golangci + CI 3 job.

**Ba chỗ lệch so với plan, có chủ ý:**

1. **Không dùng sqlc.** Phase 1 chỉ có 6 bảng nên store viết tay bằng `database/sql` gọn hơn và không thêm bước codegen. Query gom theo từng entity (`internal/db/users.go`, `sessions.go`, …) để chuyển sang sqlc sau này là cơ học. Xem lại khi schema lớn hơn ở Phase 3.
2. **Audit của agent ghi vào journald, không vào DB.** Agent chạy root, DB thuộc user `opanel`; cho agent ghi DB sẽ tạo hai writer trên cùng file SQLite mà không được gì. Kết quả là hai vệt log bổ sung nhau: journald ghi mọi thao tác đặc quyền (API không thể bỏ sót), DB ghi hành vi người dùng.
3. **Chưa có action `pkg.install`.** Cho cài package tuỳ ý là quyền lớn hơn hẳn việc đọc trạng thái, và Phase 1 không cần. Phase 2 thêm action hẹp (`php.install`…) tự mang allowlist riêng.

**Hai bug thật, chỉ lộ ra khi chạy trên máy thật** — ghi lại vì cả hai đều là loại test đơn vị không bắt được:

- **Token API không parse được.** `randomToken` dùng base64url, bảng chữ có `_`, nên `strings.Split(plain, "_")` vỡ khi prefix hoặc secret chứa ký tự đó. Sửa: prefix dùng hex, secret giữ base64url, tách bằng `SplitN(..., 3)`.
- **API không kết nối được agent socket** (`connect: permission denied`). Hai nguyên nhân chồng nhau: `BindReadOnlyPaths` làm socket chỉ đọc trong khi `connect()` cần quyền ghi trên inode, và `RuntimeDirectory` được systemd tạo theo User/Group của unit nên ra `root:root` — group `opanel` không đi vào được thư mục. Sửa: `ReadWritePaths=/run/opanel`, `Group=opanel` trên unit agent, và agent tự `chown` thư mục trong `Listen()` để chạy tay lúc cứu hộ vẫn đúng.

### ~~Phase 2 — Website chạy được~~ ✅ XONG 2026-09-14

7.583 dòng code + 1.541 dòng test. Cổng ra đã đạt và vượt: tạo user, tạo site PHP, truy cập thấy trang chạy, đổi PHP version thấy đổi theo — cộng thêm suspend/unsuspend, xoá site dọn sạch config, phân quyền theo chủ sở hữu, SFTP chroot, và rollback khi apply hỏng.

Đã có: `internal/webserver` (interface + backend OLS + golden test), `internal/phpmgr` (interface + provider lsphp), `internal/sites` (vòng đời site), `internal/platform/linuxuser`, `internal/installer` (13 bước idempotent), 11 agent action mới, API `/sites` và `/php/versions`.

**Năm phát hiện trên máy thật, mỗi cái đều đổi thiết kế:**

1. **OLS không có `include`.** Nó nhận dòng `include` mà không báo gì rồi lặng lẽ bỏ qua — vhost không được phục vụ, port không mở. Nên panel phải full-render `httpd_config.conf`. (Đúng như plan dự kiến, nhưng giờ là đã đo chứ không phải đoán.)
2. **Thư mục site phải là 0711, không phải 0750.** Worker của OLS chạy dưới tài khoản riêng chứ không phải chủ site, nên phải traverse vào được. 0750 cho 403 ở mọi request.
3. **Tên external processor phải mang PHP version.** Tên cố định ⇒ socket không đổi ⇒ graceful reload giữ nguyên tiến trình lsphp cũ ⇒ site báo version cũ mãi mãi. Đổi 8.4 sang 8.1 im lặng không có tác dụng cho tới khi version vào tên.
4. **Trang suspend không đặt được dưới `/var/lib/opanel`** — đó là state riêng của panel, mode 0750, worker không với tới. Chuyển sang `/usr/share/opanel`.
5. **Home của site owner phải là `root:<user> 0751`.** Ba ràng buộc cùng lúc: sshd từ chối `ChrootDirectory` không thuộc root hoặc group/other ghi được; worker webserver phải traverse; chủ sở hữu phải liệt kê được home của mình — 0711 cho "permission denied" ngay ở lệnh `ls` đầu tiên.

**Nhất quán khi hỏng:** backend snapshot config đang chạy, rollback + reload nếu config test hoặc reload thất bại; tầng service khôi phục cả hàng trong DB, để bản ghi và config đang phục vụ không lệch nhau. Đã kiểm bằng cách ép `lswsctrl` hỏng — config quay về bản cũ và site vẫn trả 200.

**Còn nợ sang Phase 3:** WordPress installer + WP-CLI (đã lên lịch Phase 3), và `opanelctl install` chưa được thử trên máy hoàn toàn trắng — mới thử idempotent trên máy đã cấu hình.

## ⚠ LỘ TRÌNH ĐÃ ĐỔI THỨ TỰ — 2026-09-14

Sau khi đối chiếu `docs/feature-parity.md` (6/22 tính năng v1 đã xong), thứ tự
Phase 3–6 được sắp lại theo **thời điểm bán được hàng** thay vì theo tầng kiến
trúc. Nội dung công việc không đổi, chỉ đổi thứ tự.

Nguyên tắc: một khách shared hosting tối thiểu cần **database, SSL cho site,
backup**; nhà cung cấp cần **quota và WHMCS** để thu tiền. Terminal, file
manager, quét mã độc, dashboard tài nguyên là thứ bán được mà chưa cần có ngay
từ ngày đầu.

Bảng dưới là thứ tự **thực thi**, cập nhật theo những gì đã làm xong. Thứ tự
đã lệch khỏi bản đầu tiên vì yêu cầu bổ sung: quản lý user và package phải có
trước WordPress installer, còn file manager được kéo lên trước backup.

| # | Việc | Trạng thái | Vì sao ở vị trí này |
|---|---|---|---|
| **S1** | Database MariaDB + user + phân quyền | ✅ xong | WordPress không chạy được nếu thiếu. Chặn mọi thứ phía sau |
| **S2** | Quản lý user panel + 2FA endpoint | ✅ xong | Không tạo được khách hàng thì không có gì để bán |
| **S3** | Package: giới hạn site, database, dung lượng | ✅ xong | Không có thì không ép được gói |
| **S4** | SSL cho từng site + auto-renew | ✅ xong | Không có HTTPS thì không bán được năm 2026 |
| **S5** | File manager | ✅ xong | Khách cần sửa file mà không phải mở SFTP client |
| **S6** | Backup + restore + lịch | ✅ xong | Khách không giao dữ liệu cho nhà cung cấp không có backup |
| **S7** | WordPress one-click + WP-CLI, editor PHP ini, tuner PHP/MariaDB, phpMyAdmin SSO | tiếp theo | Phần lớn khách mua hosting là để chạy WordPress |
| **S8** | Firewall API/UI, WAF, quét mã độc | | Lấp phần đang làm dở, rồi tới bảo mật |
| **S9** | Cron, dashboard tài nguyên, terminal, update OS | | Vận hành, làm sau khi đã có doanh thu |
| **S10** | API token endpoint + provisioning + module WHMCS + import DirectAdmin | | Thu tiền tự động và kéo khách từ host khác |
| **S11** | Stage B: CloudLinux + LSWS Enterprise + LVE | | Như plan cũ |

Các mục Phase 3–6 bên dưới giữ nguyên để tham chiếu nội dung; thứ tự thực thi
theo bảng trên.

---

### Phase 3 — Hạ tầng vận hành *(2 tuần)*

MariaDB provisioning + tuner + phpMyAdmin SSO; SSL bằng lego (HTTP-01 + DNS-01, auto-renew, hot reload); firewall nftables đầy đủ; WordPress installer + WP-CLI (dùng đúng PHP của site).

**Cổng ra:** tạo site WordPress một click, có SSL thật, firewall bật và sống qua reboot.

### Phase 4 — Tính năng người dùng *(3 tuần)*

File manager (`os.Root`); backup/restore + scheduler + SFTP target; cron manager; metrics dashboard; terminal websocket; WAF ModSecurity per-site; malware scan (ClamAV + LMD).

**Cổng ra:** parity với v1 cho end-user.

### Phase 5 — Frontend + provisioning *(2 tuần)*

Tách `App.jsx` thành module; nhúng vào binary; provisioning API + WHMCS contract (module PHP không đổi); hosting plan + quota.

**Cổng ra:** bán được hosting qua WHMCS: create → suspend → unsuspend → change plan → terminate → SSO.

### Phase 6 — DirectAdmin importer *(1,5 tuần)*

Port `da_import.py` (1.375 dòng). Để cuối vì phức tạp nhất và nhiều edge case, nhưng là thứ kéo khách từ host khác về nên không bỏ được.

**Cổng ra:** import archive DirectAdmin thật (files + DB + config) thành công.

### 🏁 **Stage A hoàn tất** — tổng ~11–12 tuần. Sản phẩm bán được, không phụ thuộc license nào.

---

### Phase 7 — LSWS Enterprise + switch *(2 tuần)*

Backend `lsws` (renderer XML + golden test đối chiếu ma trận với OLS); `opanelctl webserver switch`; watchdog license + auto-failover; UI banner trạng thái; `opanelctl install --webserver=lsws` và đường nâng cấp từ OLS.

**Cổng ra:** switch OLS ↔ LSWS hai chiều không mất site; **gỡ license LSWS giả lập → hệ thống tự rơi về OLS trong <3 phút, site vẫn phục vụ.**

### Phase 8 — CloudLinux + LVE *(2 tuần)*

`opanelctl cloudlinux convert` (precheck → `cldeploy -k` → hướng dẫn reboot → verify bằng `cldetect`); provider `altphp` + chuyển đổi từ `lsphp`; `internal/cloudlinux/lve`: `lvectl set` map từ hosting plan, `lveinfo --json` đọc usage; cột LVE trong bảng plan; LVE usage trên dashboard + usage snapshot WHMCS.

**Cổng ra:** convert một máy Stage A đang chạy sang CloudLinux mà không mất site; plan có LVE limit và `lveinfo` xác nhận đúng.

### 🏁 **Stage B hoàn tất** — tổng ~15–16 tuần.

---

### Để sau (không nằm trong Stage A/B)

CageFS (`/opt/cpvendor/etc/integration.ini` + 7 integration script + hooks `post_modify_user/domain/package`); MySQL Governor (`/etc/container/dbuser-map` + thay MariaDB bằng bản CloudLinux build); CloudLinux Selector cho Node.js/Python; SELinux policy module đóng gói; DNS server; email server.

---

## 6. Giả định

- **Chưa có bản opanel-ent nào chạy production.** Repo mới fork, một commit. Vì vậy schema DB làm greenfield, không cần migration từ v1. Nếu thực tế đã có máy chạy, cần thêm một importer một chiều từ SQLite cũ — báo mình biết sớm vì nó ảnh hưởng thiết kế schema.
- Module WHMCS (`modules/servers/opanelent/`) giữ nguyên PHP, chỉ cần contract API không đổi.
- VPS là KVM hoặc bare metal.

## 7. Rủi ro

| Rủi ro | Xử lý |
|---|---|
| Rewrite 26k dòng Python là công việc lớn, dễ đuối giữa chừng | Mỗi phase phải chạy được thật. Stage A đã bán được trước khi động vào CloudLinux. |
| Renderer XML LSWS Ent chưa từng viết | Golden test đối chiếu ma trận với OLS. Phase 7 nằm sau khi Stage A đã ổn định, không chặn doanh thu. |
| Switch webserver có downtime vài giây | Không tránh được (chung port 80/443). Ghi rõ trong docs, health check + rollback tự động. |
| Auto-failover flapping | Chỉ tự chuyển một chiều LSWS → OLS. Chiều ngược lại thủ công. |
| SELinux tắt sẵn trong image provider | Stage A chấp nhận, nhưng installer phải báo cáo trạng thái và code không ngầm dựa vào việc nó tắt. |
| **Stage A không có PHP 7.4** — mất tệp khách chạy code cũ | Chốt trước Phase 2: chấp nhận giới hạn 8.1+, hay kéo Stage B (alt-php 5.6→8.4 + HardenedPHP) lên sớm. |
| `openlitespeed -t` không bắt được directive sai tên | Golden-file test của renderer là lưới an toàn chính, không phải `-t`. |
| Đổi sang MySQL Governor sau này phải thay MariaDB | Lớp `mariadb` không giả định nguồn package. Chấp nhận là migration có downtime. |
| Chi phí license | CloudLinux OS **Shared** (Solo/Admin giới hạn 1 và 5 user). LSWS Ent tier cho phép unlimited domain. Chốt số trước khi định giá bán. |

## 8. Verification

**Tự động:** `go test ./...` (golden file cho cả hai renderer, nftables ruleset, path confinement, tier tuner), `golangci-lint`, build `CGO_ENABLED=0` cho amd64 + arm64.

**Trên VPS thật, sau mỗi phase:**

1. Cài sạch AlmaLinux 10 → một lệnh → panel lên, login được
2. Tạo user + site WordPress → chạy, `.htaccess` rewrite đúng
3. Đổi PHP 8.4 → 7.4 → 8.4, `phpinfo()` khớp
4. SSL Let's Encrypt cấp được + renew dry-run + hot reload không rớt kết nối
5. Backup → xoá site → restore
6. Firewall: bật, thêm rule, block IP, **reboot**, rule còn nguyên
7. WHMCS: create → suspend → unsuspend → change plan → terminate → SSO
8. Import archive DirectAdmin
9. *(Phase 7)* Switch OLS → LSWS → OLS, site không mất. **Gỡ license → tự failover < 3 phút**
10. *(Phase 8)* Convert sang CloudLinux → site còn nguyên → gán LVE limit → `lveinfo` xác nhận

---

## Nguồn tham khảo

- [CloudLinux Installation / cldeploy](https://docs.cloudlinux.com/cloudlinuxos/cloudlinux_installation/)
- [CloudLinux OS 10 cho non-panel / custom panel](https://blog.cloudlinux.com/cloudlinux-os-10-is-now-available-for-non-panel-and-custom-panel-installations)
- [CloudLinux Control Panel Integration](https://docs.cloudlinux.com/cloudlinuxos/control_panel_integration/)
- [Sau convert, os-release vẫn báo AlmaLinux](https://cloudlinux.zendesk.com/hc/en-us/articles/24859875769500-AlmaLinux-10-to-CloudLinux-10-conversion-etc-os-release-still-reports-AlmaLinux)
- [LSWS Enterprise standalone install](https://docs.litespeedtech.com/lsws/standalone/)
- [MariaDB 11.8 LTS](https://mariadb.org/11-8-lts-released/) · [Repo setup cho RHEL 10](https://mariadb.com/docs/server/server-management/install-and-upgrade-mariadb/mariadb-package-repository-setup-and-usage)
- [ipset và iptables-nft đã deprecated](https://access.redhat.com/solutions/6739041)
