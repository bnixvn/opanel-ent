# Plan v3 — từ "chạy được" sang "bán được"

Bản này nối tiếp `docs/PLAN-v2-golang.md`. v2 đã đưa panel tới chỗ vận hành
được (S1–S7 đầu, xem `docs/feature-parity.md`). Bản v3 là danh sách yêu cầu
chi tiết do chủ sản phẩm đưa ra ngày 2026-09-14, đã bóc thành thiết kế và thứ
tự thực thi.

---

## 0. Bốn quyết định đã chốt

| Chủ đề | Quyết định | Hệ quả |
|---|---|---|
| Frontend | **React + Vite, viết mới UI** (không port code UI v1) | Thêm bước build node vào pipeline; UI file manager phải viết lại, không dùng lại `file-manager.css` của v1 |
| Quota | **Cứng, bằng XFS project quota** | Cần `rootflags=pquota` + **một lần reboot** VPS |
| Subdomain | **Subdomain là một website** | Không phát sinh bảng/khái niệm mới; quota, backup, SSL, file manager dùng lại nguyên |
| Reseller | **Có hạn mức riêng để chia lại** | Đổi mô hình dữ liệu: reseller sở hữu khách, có pool tài nguyên, mọi endpoint listing phải scope lại |

## 1. Dữ kiện đã xác minh trên host (2026-09-14)

Đo trực tiếp, không suy đoán:

| Dữ kiện | Kết quả | Ảnh hưởng |
|---|---|---|
| Filesystem gốc | `/dev/sda4` XFS, mount `noquota`; `/home` **không** phải mount riêng | Project quota phải bật ở kernel cmdline, không remount được |
| Công cụ quota | `xfs_quota`, `quota`, `setquota`, `xfsprogs 6.11`, `grubby` đều có | Không cần cài gì thêm |
| Node | Chưa cài; `nodejs 22.23.2` có trong appstream | Đủ cho Vite (v1 yêu cầu ≥22.12) |
| ClamAV | `clamav 1.4.6` + `clamd` trong EPEL | Khả thi |
| ModSecurity | **`mod_security.so` đã có sẵn** trong `/usr/local/lsws/modules/`; package `ols-modsecurity 1.9.2` cũng có | Không phải cài, chỉ cấu hình + nạp ruleset |
| ionCube | **Đã được load sẵn** trong lsphp (`php -m` liệt kê `ionCube Loader`) | Chỉ cần phơi trạng thái ra UI, không phải cài |
| PHP CLI | `/usr/local/lsws/lsphpNN/bin/php`, có mysqli/curl/zip/mbstring/intl/gd | Đủ cho WP-CLI, cron, phpMyAdmin |

## 2. Những gì đã có, không phải làm lại

Đối chiếu với yêu cầu, các mục sau đã xong trong v2 và chỉ cần đưa lên UI mới:

- Mỗi panel user là một Linux user, home riêng ở `/home/<user>`, đồng thời là tài khoản SFTP có chroot — **đã xong và đã kiểm chứng cô lập**
- Site WordPress/PHP/static, multi-PHP theo từng site, `.htaccess` đầy đủ
- Backup/restore theo tài khoản, có lịch và retention
- WordPress one-click + WP-CLI
- SSL Let's Encrypt HTTP-01 cho từng site + auto-renew
- Audit log hai lớp, 2FA TOTP, API token

## 3. Nền tảng — phải làm trước, vì chạm vào mọi thứ

### 3.1 Mô hình reseller có hạn mức (R1)

Hiện `sites.VisibleTo()` trả 0 cho reseller, nghĩa là reseller thấy toàn server.
Phải đổi thành quan hệ sở hữu thật.

**Dữ liệu mới**

```sql
ALTER TABLE users ADD COLUMN parent_id INTEGER REFERENCES users(id);

CREATE TABLE reseller_limits (
    user_id           INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_accounts      INTEGER NOT NULL DEFAULT 0,   -- 0 = không giới hạn
    max_sites         INTEGER NOT NULL DEFAULT 0,
    max_databases     INTEGER NOT NULL DEFAULT 0,
    disk_quota_mb     INTEGER NOT NULL DEFAULT 0,
    bandwidth_gb      INTEGER NOT NULL DEFAULT 0,
    can_create_plans  INTEGER NOT NULL DEFAULT 1
);
```

`plans` thêm `owner_id`: gói do admin tạo là gói hệ thống, gói do reseller tạo
chỉ reseller đó thấy và chỉ gán được cho khách của mình.

**Quy tắc phân quyền**

| Hành động | Admin | Reseller | End user |
|---|---|---|---|
| Xem/sửa mọi tài khoản | ✔ | chỉ khách có `parent_id` = mình | chỉ chính mình |
| Tạo tài khoản | ✔ | ✔, trong `max_accounts` và tổng quota chưa vượt `disk_quota_mb` | ✘ |
| Tạo gói | ✔ (gói hệ thống) | ✔ nếu `can_create_plans`, gói riêng | ✘ |
| Đặt hạn mức reseller | ✔ | ✘ | ✘ |
| Login as user | ✔ mọi người | ✔ chỉ khách của mình | ✘ |
| Firewall, WAF, PHP version, Settings, Update | ✔ | ✘ (chỉ xem) | ✘ |
| Malware scan | toàn server | chỉ khách của mình | chỉ site của mình |
| Cron | thấy tất cả, xoá được | khách của mình | của mình |

**Cưỡng chế tổng hạn mức**: khi reseller tạo tài khoản hoặc gán gói, tính tổng
`disk_quota_mb` đã cấp cho khách của mình; vượt `reseller_limits.disk_quota_mb`
thì từ chối. Tính **hạn mức đã cấp**, không phải dung lượng đang dùng — bán
hàng thì oversell là quyết định kinh doanh, không phải lỗi kỹ thuật; nhưng
mặc định chặn, có cờ `allow_oversell` cho admin bật cho từng reseller.

**Ảnh hưởng**: mọi handler listing (`sites`, `databases`, `users`, `backups`,
`files`, `plans`, `cron`, `logs`) phải đổi từ "reseller thấy tất cả" sang
"reseller thấy cây con của mình". Đây là lý do phải làm trước frontend.

### 3.2 Quota cứng bằng XFS project quota (R1)

**Cơ chế**: mỗi Linux user được cấp một project id (dùng luôn uid cho dễ đối
chiếu), gán vào thư mục home, đặt hard limit. Kernel từ chối ghi khi vượt —
kể cả qua SFTP, PHP hay cron, không phụ thuộc panel có kiểm tra hay không.

**Các bước cài** (thành một installer step riêng, idempotent):

1. `grubby --update-kernel=ALL --args="rootflags=pquota"`
2. Yêu cầu người vận hành reboot — **installer không tự reboot**, chỉ báo và
   ghi trạng thái "chờ reboot" để panel hiển thị banner
3. Sau reboot: kiểm tra `findmnt -no OPTIONS /` có `prjquota`
4. `/etc/projects` và `/etc/projid` do agent quản lý, ghi lại từ database
5. `xfs_quota -x -c 'project -s <name>' -c 'limit -p bhard=<N>m <name>' /`

**Fallback**: nếu chưa reboot, panel vẫn chạy, quota rơi về chế độ mềm như
hiện tại và UI ghi rõ "quota chưa được cưỡng chế, cần reboot". Không được im
lặng — bán hàng với quota tưởng là cứng mà thực ra mềm là tệ hơn cả không có.

**Đo dung lượng**: sau khi có project quota, `xfs_quota -c 'quota -p'` trả số
liệu tức thì, bỏ hẳn `du -sb` đang chạy tốn I/O.

**Băng thông**: yêu cầu có nhắc `bandwidth_gb` ở gói. Nguồn dữ liệu là access
log của OLS (cột bytes). Gom theo tháng bằng một job đọc log, lưu vào bảng
`bandwidth_usage(owner_id, month, bytes)`. Vượt thì cảnh báo, tuỳ chọn suspend.

### 3.3 Frontend viết mới: React + Vite, sidebar, route thật (R2)

**Cấu trúc**

```
web/
  package.json          react 18, vite 8, react-router, lucide-react,
                        ace-builds (editor), @xterm/xterm (terminal sau này)
  src/
    main.jsx            router
    api.js              fetch wrapper, xử lý 401 và impersonation banner
    layout/Sidebar.jsx  menu trái, ẩn mục theo role
    pages/
      Dashboard.jsx  Sites.jsx  SiteDetail.jsx  Databases.jsx
      Files.jsx      Backups.jsx  Ssl.jsx      Security/Firewall.jsx
      Security/Waf.jsx  Logs.jsx  Malware.jsx  Cron.jsx  Php.jsx
      Users.jsx      Plans.jsx    Account.jsx  Settings.jsx  Update.jsx
```

**Đường dẫn riêng cho từng mục**: `/sites`, `/sites/:id`, `/databases`,
`/files`, `/backups`, `/ssl`, `/security/firewall`, `/security/waf`, `/logs`,
`/malware`, `/cron`, `/php`, `/users`, `/plans`, `/account`, `/settings`,
`/update`. Backend đã có sẵn deep-link fallback trả `index.html` cho mọi
đường dẫn ngoài `/api`, nên không phải sửa gì thêm ở đó.

**Ô search ajax** cho list website và list database: lọc phía client khi dưới
200 dòng, và endpoint nhận `?q=` để lọc phía server khi vượt — một server bán
hosting sẽ vượt.

**Đóng gói**: `vite build` ra `web/dist`, `go:embed` vào binary. Thêm bước
build vào `Makefile` và vào quy trình release. Node chỉ cần trên máy build,
**không** cần trên host khách.

**Branding**: brandname, logo, favicon đọc từ `/api/settings/branding`, áp
bằng CSS variable + thẻ `<link rel=icon>` động, không hard-code.

### 3.4 Settings + thông báo (R2, đi cùng frontend)

- **Panel URL**: đổi hostname/port, cài SSL cho hostname đó (Let's Encrypt),
  hiển thị IP v4/v6 của server
- **SNI**: panel giữ một cert store; `tls.Config.GetCertificate` chọn cert
  theo SNI. Cho phép đăng nhập panel bằng **bất kỳ domain nào đã có SSL trong
  hệ thống** — kể cả cert của site khách. Cần một danh sách trắng hostname
  được phép, nếu không thì bất kỳ ai trỏ domain vào IP cũng vào được panel
- **IPv6**: tự nhận diện địa chỉ v6, bật/tắt việc panel + OLS lắng nghe v6,
  kèm rule nftables `ip6`
- **Thông báo Telegram**: `notification_settings(user_id, telegram_bot_token,
  telegram_chat_id, events)`. Admin và reseller mỗi người cấu hình riêng.
  Sự kiện: backup hỏng, quota vượt ngưỡng 90%, phát hiện mã độc, gia hạn cert
  hỏng, disk server sắp đầy, service chết

---

## 4. Đặc tả từng mục

### 4.1 Database (R5)

- **Tạo DB là tạo user luôn**: một form, sinh cả `<owner>_<name>` và
  `<owner>_<name>` user + grant, trả password một lần. Vẫn giữ API tạo riêng
  lẻ cho trường hợp cần user thứ hai (ví dụ tài khoản chỉ đọc)
- **phpMyAdmin**: EL không có package → vendor tarball vào `/usr/share/phpmyadmin`,
  verify checksum, sinh `config.inc.php` với `auth_type = signon`
- **SSO**: panel sinh token dùng một lần hạn 60 giây (v1 đã có
  `sso_tokens.py`, port lại cơ chế), redirect sang một shim PHP; shim gọi
  ngược API panel để đổi token lấy cặp user/password rồi nạp vào `$_SESSION`
  theo giao thức signon của phpMyAdmin. Không bao giờ đặt mật khẩu vào URL
- **Download database**: dùng lại `mariadb-dump` của backup, stream về trình
  duyệt qua staging file như file manager đang làm

### 4.2 File manager đầy đủ (R6)

Giữ nguyên cơ chế cô lập `os.Root` đã có (kernel chặn cả symlink escape), bổ
sung các thao tác v1 có mà v2 chưa:

| Thao tác | Ghi chú thiết kế |
|---|---|
| Copy / Move / Paste | Clipboard ở phía client; agent nhận danh sách nguồn + đích, thao tác trong cùng một `os.Root` |
| Archive | zip và tar.gz; giới hạn tổng dung lượng nguồn để một khách không nén 50GB làm nghẽn server |
| Extract | Chống zip-slip bằng `os.Root`, kiểm tra dung lượng giải nén trước khi ghi (`_zip_uncompressed_size` của v1 chống zip bomb — port logic này) |
| Permission | Đã có; bổ sung recursive cho thư mục |
| Editor | Ace editor, syntax highlight theo đuôi file, trần 1MB như hiện tại |
| Chọn nhiều file | Checkbox + thao tác hàng loạt |
| Upload nhiều file / kéo thả | Đã có |

Phạm vi: v1 giới hạn theo **website**, v2 hiện theo **home của tài khoản**.
Giữ theo home — khách có nhiều site cần copy file giữa các site.

### 4.3 Backup mở rộng (R11)

- **Destination**: ngoài local, thêm **SFTP** (`pkg/sftp`) và **S3**
  (`aws-sdk-go-v2`, dùng được cả S3 tương thích như Wasabi/R2/Backblaze).
  Bảng `backup_destinations(id, owner_id, kind, config_json, enabled)`,
  secret mã hoá bằng khoá panel chứ không lưu thô
- **Restore từ DirectAdmin**: v1 đã có `da_import.py` 1.375 dòng — port sang
  Go, giữ nguyên logic đọc `backup/user.conf`, `domains/`, `*.sql`
- **Restore từ cPanel**: định dạng `cpmove-<user>.tar.gz`. Đọc `userdata/`,
  `homedir/`, `mysql.sql`, `cp/<user>`. Không có mail/DNS trong panel nên bỏ
  qua hai phần đó và **báo rõ trong kết quả import** những gì đã bỏ, thay vì
  im lặng
- **Backup/restore theo user**: đã có; bổ sung nút "restore sang tài khoản
  khác" cho admin (đổi owner khi giải nén)

### 4.4 PHP (R7)

- **Editor thông số**: form cho các directive khách thực sự cần —
  `memory_limit`, `upload_max_filesize`, `post_max_size`, `max_execution_time`,
  `max_input_vars`, `max_input_time`, `display_errors`, `date.timezone`,
  `opcache.enable`. Whitelist cứng: directive ngoài danh sách bị từ chối, vì
  `disable_functions` hay `open_basedir` mà cho khách sửa là cho khách tự mở
  cửa
- **Phạm vi**: theo **từng site**, render vào `phpIniOverride` của vhost OLS.
  Thêm cột `php_settings` (JSON) trên bảng `sites`
- **Mặc định theo gói**: gói có thể đặt trần, khách chỉ chỉnh trong trần đó
- **ionCube**: đã load sẵn. Chỉ cần trang PHP hiển thị trạng thái loader theo
  từng version (đọc từ `php -m`) để khách yên tâm cài plugin WP cần nó

### 4.5 Login as user (R10)

- `POST /api/users/{id}/impersonate` → tạo session mới cho user đích, cột
  `impersonated_by` trên bảng `sessions`
- UI hiện banner cố định "Đang xem với tư cách <user> — Thoát", không thể bỏ qua
- Audit cả lúc vào và lúc ra
- Reseller chỉ impersonate được khách của mình; **không ai** impersonate được
  admin khác

### 4.6 Account: passkey + đổi email (R10)

- **Passkey**: WebAuthn qua `go-webauthn/webauthn`. Bảng `webauthn_credentials`.
  Cho đăng ký nhiều passkey, đặt tên từng cái, thu hồi từng cái
- **Ràng buộc**: WebAuthn cần RP ID là **domain**, không chạy trên IP trần.
  Nên phần này phụ thuộc mục Settings/panel URL — làm sau, và UI phải nói rõ
  "cần đặt domain cho panel trước"
- **Đổi email**: email tài khoản dùng làm contact ACME. Đổi email thì xác
  nhận qua mật khẩu hiện tại; không gửi mail xác nhận vì panel chưa có SMTP

### 4.7 Security → Firewall (R4)

Ruleset nftables đã dựng lúc cài, giờ thêm quản lý:

- **Cổng**: mở/đóng, theo tcp/udp, có mô tả. Cổng 22/80/443/2222 được đánh dấu
  "bảo vệ" — đóng thì phải xác nhận hai lần, và dead-man switch đã có sẵn sẽ
  khôi phục nếu người vận hành tự khoá mình ra ngoài
- **Chặn IP / dải IP**: nft set `blocklist4` / `blocklist6`, `flags interval`
  nên nhận cả CIDR
- **Allow IP**: set riêng, xét trước blocklist
- **Blocklist từ URL .txt**: bảng `blocklist_sources(url, enabled, interval,
  last_fetch, entry_count)`. Job định kỳ tải, parse (một IP/CIDR mỗi dòng, bỏ
  qua dòng `#`), nạp bằng `nft -f` với `flush set` — atomic, không có khoảng
  trống nào firewall bị rỗng. Giới hạn số dòng để một file 10 triệu dòng không
  làm chết server

### 4.8 Security → WAF (R4)

- `mod_security.so` đã có sẵn trong OLS
- Nạp **OWASP Core Rule Set**, vendor vào `/usr/local/lsws/conf/waf/`
- Bật/tắt **theo từng site** (cột `waf_enabled` đã có trong DB, đã render ra
  vhost — chỉ còn thiếu engine phía sau)
- Ba chế độ: tắt / chỉ ghi log (`DetectionOnly`) / chặn. Mặc định **ghi log**
  khi mới bật cho một site, vì CRS chặn nhầm là chuyện thường và một site
  WordPress chết vì WAF sẽ thành ticket ngay
- Trang WAF hiển thị các request bị chặn gần đây (đọc audit log của modsec),
  cho phép thêm rule ID vào danh sách loại trừ theo từng site

### 4.9 Logs (R3)

- OLS đã ghi log riêng cho từng vhost ở `/home/<user>/<domain>/logs/`
- API: `GET /api/logs?site=<id>&kind=access|error&lines=N` (tail) và
  `GET /api/logs/download?...`
- Phân quyền theo chủ site, dùng lại đúng cơ chế của file manager
- UI: chọn website, chọn access/error, xem N dòng cuối, ô lọc, nút tải về

### 4.10 SSL — bốn cách cài (R3)

Đây là mục v1 làm khá đầy đủ (`ssl.py`, 559 dòng), port lại hành vi:

1. **Let's Encrypt cho website**: đã có HTTP-01. Bổ sung: khi cấp cho
   `domain.com`, **tự kiểm tra `www.domain.com` có trỏ về server không**; có
   thì thêm vào SAN, không thì bỏ qua và nói rõ — chứ không để cả đơn hàng
   hỏng vì một alias sai DNS
2. **Wildcard bằng Cloudflare token**: DNS-01 qua lego provider `cloudflare`.
   Token lưu mã hoá, phạm vi tối thiểu (`Zone:DNS:Edit`). Cert `*.domain.com`
   + `domain.com`
3. **Reuse existing cert**: khi thêm `xyz.domain.com`, nếu trong cert store đã
   có cert phủ được tên đó (wildcard hoặc SAN), cho chọn dùng lại. Đúng với
   quyết định "subdomain là một website"
4. **Manual SSL**: upload cert + key + CA bundle. Validate như v1: key khớp
   cert, cert còn hạn, CN/SAN phủ domain — từ chối trước khi ghi file, không
   để một cert sai làm OLS không khởi động lại được

Cert store dùng chung một chỗ, có bảng chỉ mục để mục 3 và SNI của panel cùng
tra cứu được.

### 4.11 Malware Scanner (R8)

- **ClamAV** từ EPEL, `clamd@scan` + `freshclam`
- Quét **theo từng website** hoặc **toàn bộ**, chạy nền như backup (job row +
  poll), vì quét vài GB mất hàng chục phút
- **Cô lập**: file bị phát hiện chuyển vào `/var/lib/opanel/quarantine/<owner>/`,
  giữ nguyên đường dẫn gốc trong metadata để khôi phục được. **Không xoá** —
  false positive của ClamAV trên mã PHP là chuyện thường
- **Đặt lịch**: giống scheduler backup, dùng lại cơ chế `Due()`
- **Nâng cao (giai đoạn sau)**: Linux Malware Detect + inotify để bảo vệ
  realtime. Ghi rõ đây là bước hai vì LMD + inotify trên server nhiều file
  ăn RAM đáng kể, cần đo trước

### 4.12 Cron (R9)

- Cron chạy dưới **Linux user của khách**, không phải một user chung
- Validate câu lệnh: chỉ cho phép chạy PHP của site, `curl`/`wget` tới chính
  domain của mình, và WP-CLI — như v1 đã làm. Cho chạy lệnh tuỳ ý là cho
  khách một shell
- Admin xem toàn bộ cron của mọi user và xoá được; reseller xem của khách mình
- UI: schedule dạng preset (mỗi 5 phút / hàng giờ / hàng ngày lúc…) kèm ô
  nhập cron expression cho người biết việc

### 4.13 Update panel (R10)

- Nguồn: GitHub release/tag của repo gốc (v1 dùng `opanel_ent_REPO_URL`)
- Luồng: kiểm tra release mới → tải tarball → verify checksum → giải nén vào
  thư mục tạm → chạy migration → thay binary → restart agent rồi API
- **Rào chắn**: binary đang chạy tự thay chính nó. Ghi phiên bản cũ sang
  `/usr/local/bin/*.prev`, và nếu health check sau restart hỏng trong 30 giây
  thì tự quay lại bản cũ
- Có cả bản thủ công (nút Update) và tự động theo lịch

---

## 5. Thứ tự thực thi

Sắp theo phụ thuộc, không theo mức độ hấp dẫn.

| # | Khối | Vì sao ở đây | Ước lượng |
|---|---|---|---|
| **R1** | Reseller + hạn mức, quota cứng XFS | Đổi mô hình dữ liệu và mọi endpoint listing. Làm sau frontend là phải sửa frontend hai lần | 5–7 ngày |
| **R2** | Frontend React+Vite: sidebar, route, search, branding, Settings | Mọi tính năng sau đều cần chỗ để hiển thị. Làm muộn là viết UI hai lần | 7–10 ngày |
| **R3** | SSL bốn cách + Logs | Giá trị cao nhất cho khách, độc lập với phần còn lại | 4–5 ngày |
| **R4** | Security: Firewall API/UI + WAF | Lấp phần đang làm dở, và là thứ khách hỏi khi so sánh với cPanel | 5–6 ngày |
| **R5** | Database: tạo kèm user, phpMyAdmin + SSO, download | Hoàn thiện mục đang thiếu rõ nhất | 3–4 ngày |
| **R6** | File manager đầy đủ | Nền `os.Root` đã có, chủ yếu là thao tác mới + UI | 4–5 ngày |
| **R7** | PHP config theo site + trạng thái ionCube | Nhỏ, nhưng khách WordPress nào cũng cần | 2–3 ngày |
| **R8** | Malware scanner ClamAV | Chạy nền, dùng lại cơ chế job của backup | 3–4 ngày |
| **R9** | Cron | Nhỏ và độc lập | 2 ngày |
| **R10** | Login as user, passkey, đổi email, Update panel | Passkey phụ thuộc panel có domain (R2) | 4–5 ngày |
| **R11** | Backup destination SFTP/S3, import DirectAdmin + cPanel | Nặng nhất, và chỉ cần khi bắt đầu kéo khách từ host khác | 7–9 ngày |

Tổng thô: **46–60 ngày công**, chưa tính Stage B (CloudLinux + LSWS Enterprise).

## 6. Rủi ro và điểm còn phải chốt

| Rủi ro | Xử lý |
|---|---|
| Reboot để bật project quota | Việc của người vận hành, không tự động. Panel hiển thị banner "quota chưa cưỡng chế" cho tới khi thấy `prjquota` trong mount options |
| SNI cho panel: bất kỳ domain nào trỏ vào IP đều vào được panel | Bắt buộc có danh sách trắng hostname, mặc định chỉ hostname panel |
| CRS chặn nhầm làm chết site khách | Mặc định `DetectionOnly` khi bật lần đầu cho mỗi site; có trang xem request bị chặn và loại trừ theo rule ID |
| Cloudflare token có quyền rộng | Hướng dẫn tạo token phạm vi `Zone:DNS:Edit` cho đúng zone; lưu mã hoá; không hiển thị lại sau khi lưu |
| Import cPanel: không có mail/DNS trong panel | Báo rõ trong kết quả import phần nào đã bỏ qua, không im lặng |
| Tự update binary đang chạy | Giữ bản `.prev`, health check 30 giây, tự rollback |
| LMD realtime ăn RAM | Để giai đoạn sau, đo trước khi bật mặc định |

**Còn phải chốt sau**: có làm email hosting không (hiện không có trong phạm
vi, nhưng khách chuyển từ cPanel sẽ hỏi), và có làm quản lý DNS không.
