# Phase 0 — Kết quả bring-up trên AlmaLinux 10

Máy khảo sát: `oe.sgd.ovh` — `160.236.192.84`, AlmaLinux **10.1** (Heliotrope Lion), kernel `6.12.0-124.55.3.el10_1.x86_64`, KVM, 2 vCPU / 1.9 GiB RAM / 20 GB SSD (xfs).
Ngày chạy: 2026-09-14. Script: `installer/phase0-recon.sh`.

Tài liệu này chốt các giả định của `docs/PLAN-v2-golang.md`. Mục nào lệch so với plan được đánh dấu **⚠ SỬA PLAN**.

---

## Trả lời 7 câu hỏi của Phase 0

| # | Câu hỏi | Kết quả |
|---|---|---|
| 1 | lsphp nào có trên el10, thư mục ini scan thật | **Chỉ 8.1–8.5. Không có 7.4.** Ini scan: `/usr/local/lsws/lsphp{NN}/etc/php.d/` |
| 2 | MariaDB 11.8 từ repo chính thức | ✅ `MariaDB-server` 11.8.9, chạy tốt |
| 3 | nftables / `nft -j` / firewalld / iptables | ✅ nft 1.1.5, `-j` OK. Máy sạch: không có firewalld, không có iptables |
| 4 | SELinux + label webroot | **Disabled** sẵn trong image |
| 5 | valkey service + config path | ✅ `valkey.service`, `/etc/valkey/valkey.conf`, CLI là `valkey-cli` |
| 6 | Node.js + Go toolchain | ✅ Go **1.26.7** và Node **22.23.2** ngay trong repo AlmaLinux |
| 7 | KVM hay container | ✅ **KVM** — Stage B (LVE/CageFS) khả thi |

---

## 1. ⚠ SỬA PLAN — Multi-PHP Stage A chỉ có 8.1 trở lên

Repo LiteSpeed cho el10 (`http://rpms.litespeedtech.com/centos/10/x86_64/`) chỉ có:

```
lsphp81  lsphp82  lsphp83  lsphp84  lsphp85
```

**Không có `lsphp74`, không có `lsphp80`.** Repo `litespeed-edge` không tải được metadata (404).

Đây là phát hiện có ảnh hưởng thương mại, không chỉ kỹ thuật:

- **Stage A bán được cho khách dùng PHP ≥ 8.1.** Khách còn chạy WordPress/plugin cũ hoặc Laravel cũ trên PHP 7.4 thì Stage A **không phục vụ được**.
- Đường ra duy nhất là **Stage B**: CloudLinux alt-php có từ 5.6 tới 8.4 kèm **HardenedPHP** (vá bảo mật cho bản đã EOL). Đây chính là lý do mạnh nhất để làm Stage B, mạnh hơn cả LVE.
- Cần quyết định: chấp nhận Stage A giới hạn 8.1+, hay đẩy Stage B lên sớm hơn trong lộ trình.

`phpmgr.SupportedVersions` cho provider `lsphp` = `{8.1, 8.2, 8.3, 8.4, 8.5}`.

Ghi chú: cài `openlitespeed` tự kéo theo `lsphp83` làm dependency.

## 2. ⚠ SỬA PLAN — `openlitespeed -t` có exit code bất thường

Đo thực tế trên 5 trường hợp:

| Trường hợp | exit | `[ERROR]` |
|---|---|---|
| Config hợp lệ | **1** | 0 |
| Directive lạ (`this_is_garbage {`) | **1** | 0 |
| `configFile` trỏ file không tồn tại | **2** | 2 |
| Listener trùng port 8088 | **1** | 0 |

Hai kết luận bắt buộc phải đưa vào code:

1. **`exit == 1` là TRẠNG THÁI THÀNH CÔNG**, không phải 0. Ai viết `if rc == 0` sẽ coi mọi config hợp lệ là hỏng.
2. **OLS bỏ qua directive lạ, chỉ cảnh báo.** Một lỗi chính tả trong renderer sẽ không bị `-t` bắt — nó lặng lẽ không có tác dụng. Duplicate listener cũng lọt.

Cổng kiểm tra đúng:

```go
// Đúng: rc==2 hoặc có [ERROR] mới là hỏng.
ok := rc != 2 && !bytes.Contains(out, []byte("[ERROR]"))
```

Và vì `-t` không bắt được directive sai tên, **golden-file test cho renderer là lưới an toàn thật sự**, không phải `-t`. Điều này nâng mức ưu tiên của bộ test renderer trong Phase 2.

## 3. ⚠ SỬA PLAN — sshd: Match block phải ở CUỐI file chính

Plan trước viết sai. Thực tế:

```
/etc/ssh/sshd_config:15:  Include /etc/ssh/sshd_config.d/*.conf
/etc/ssh/sshd_config:40:  PermitRootLogin yes
/etc/ssh/sshd_config:65:  PasswordAuthentication yes
/etc/ssh/sshd_config:123: Subsystem sftp /usr/libexec/openssh/sftp-server
```

`Include` nằm ở **dòng 15, trên đầu**. Hệ quả:

- Muốn **ghi đè directive toàn cục** → đặt file trong `sshd_config.d/` sắp xếp trước (`10-opanel.conf`), vì sshd lấy giá trị đầu tiên gặp được.
- Nhưng **tuyệt đối không đặt `Match` block trong `sshd_config.d/`**: `Match` có hiệu lực tới `Match` kế tiếp hoặc hết file. Include ở dòng 15 nghĩa là toàn bộ phần còn lại của file chính (dòng 16–130) sẽ rơi vào trong `Match` đó.
- → **Match block cho SFTP chroot phải append vào cuối `/etc/ssh/sshd_config`.**

File có sẵn trong `sshd_config.d/`: `40-redhat-crypto-policies.conf`, `50-cloud-init.conf`, `50-redhat.conf`.

Cũng lưu ý `Subsystem sftp` đang trỏ `/usr/libexec/openssh/sftp-server`. Chroot SFTP cần đổi sang `internal-sftp`.

## 4. Trình quản lý gói — dnf4, không phải dnf5

`dnf 4.20.0`, package `dnf5` không cài. **`dnf groupinstall` giữ nguyên cú pháp.** Giả định dnf5 trong plan là sai, bỏ.

EPEL 10 đã có sẵn (`epel-release-10-8`, script LiteSpeed tự nâng cấp từ 10-6).

Repo bật sẵn: `baseos`, `appstream`, `crb`, `extras`, `epel`.

## 5. Gói trong repo — những cái khác dự đoán

| Gói | Trạng thái | Ghi chú |
|---|---|---|
| `valkey` | có 8.0.11 | ✅ |
| `redis` | ❌ không có | Đúng như plan |
| `nftables` | có 1.1.5 | ✅ |
| `iptables` | ❌ không có | Lệnh `iptables` đến từ `iptables-nft` |
| `iptables-nft` | có 1.8.11 | Vẫn còn, nhưng v2 không dùng |
| `ipset` | có 7.22 | Vẫn còn, không dùng |
| `firewalld` | có, **chưa cài** | Máy sạch, không phải disable gì |
| `mariadb-server` (AppStream) | 3:10.11.18 | **Không phải 11.x** → bắt buộc dùng repo MariaDB |
| `MariaDB-server` (repo chính thức) | 11.8.9 | ⚠ **tên viết hoa**, khác `mariadb-server` |
| `golang` | 1.26.7 | Build ngay trên máy được |
| `nodejs` | 22.23.2 | Không cần NodeSource |
| `npm` | ❌ | Nằm trong `nodejs-npm` 10.9.7 |
| `dnf-automatic` | đã cài sẵn | |
| `quota` | đã cài sẵn | |
| `clamav` | có 1.4.6 | EPEL |
| `certbot` | có 4.2.0 | Không dùng — v2 dùng lego in-process |
| `policycoreutils-python-utils` | có | Cần nếu bật SELinux (cung cấp `semanage`, `audit2allow`) |
| `lsphp{NN}-ioncube` | **có** | ⚠ Xem mục 6 |

## 6. ionCube có sẵn qua RPM — bỏ hẳn installer thủ công

`lsphp84-ioncube` cài được từ repo, tự sinh `/usr/local/lsws/lsphp84/etc/php.d/01-ioncube_loader.ini`, và `php -m` xác nhận `ionCube Loader` đã nạp.

→ Bỏ hoàn toàn phần tải tarball ionCube. Đây là ~110 dòng của v1 (`install.sh:305-366` + `helper.sh:1588-1638`) biến mất.

## 7. Layout OpenLiteSpeed

```
/usr/local/lsws/lsphp{81,83,84}/          mỗi version một cây riêng
  bin/{lsphp,php,php-cgi,phpize,phar}
  etc/php.ini                             ← Loaded Configuration File
  etc/php.d/                              ← Scan dir  ⇒ phpmgr.IniDropIn
/usr/local/lsws/conf/                     lsadm:nobody  0750
  httpd_config.conf                       config chính
  vhosts/<NAME>/vhconf.conf               config từng vhost
  templates/{ccl,rails}.conf              template vhost, có $VH_NAME / $VH_ROOT
  cert/
/usr/local/lsws/bin/{openlitespeed,lswsctrl}
```

- **`phpmgr.IniDropIn(v)` = `/usr/local/lsws/lsphp{NN}/etc/php.d/99-opanel.ini`** — chốt.
- Thư mục conf thuộc `lsadm:nobody` mode 0750 → agent ghi xong **phải `chown lsadm:nobody`**.
- Service: `lshttpd.service` (enabled sẵn), `lsws.service` và `openlitespeed.service` là alias. Cả ba tên đều dùng được.
- `lswsctrl` có `restart` = graceful zero-downtime, và `fullrestart` = stop rồi start.
- Mặc định nghe **8088** (HTTP) và **7080** (WebAdmin) — installer phải đổi sang 80/443.

**Cách vhost được nối vào config chính:**

```
virtualhost Example {
  vhRoot      Example/
  configFile  conf/vhosts/Example/vhconf.conf
}
listener Default {
  ...
  map  Example  *
}
```

→ Thêm một site **phải sửa `httpd_config.conf`** (thêm block `virtualhost` + dòng `map` trong listener). Xác nhận thiết kế full-render trong plan là cần thiết. OLS có sẵn cơ chế template (`$VH_NAME`) có thể giảm lặp, nhưng vì mỗi site khác PHP version nên vẫn cần override trong `vhconf.conf` — giữ full-render cho đơn giản và dễ test.

## 8. MariaDB 11.8

```
mariadbd Ver 11.8.9-MariaDB
socket    /var/lib/mysql/mysql.sock        ← không phải /run/mysqld/mysqld.sock của Debian
config    /etc/my.cnf.d/{server,client,mysql-clients,spider}.cnf
service   mariadb.service
```

Cài bằng `dnf install MariaDB-server MariaDB-client` (viết hoa) sau khi thêm repo:

```bash
curl -LsS https://r.mariadb.com/downloads/mariadb_repo_setup \
  | bash -s -- --os-type=rhel --os-version=10 --mariadb-server-version=mariadb-11.8
```

Đã `systemctl enable --now mariadb`, `SELECT VERSION()` trả `11.8.9-MariaDB`. Root auth qua unix socket, chạy được ngay bằng `mariadb -e`.

Tuner ghi `/etc/my.cnf.d/99-opanel.cnf`.

## 9. Valkey

```
service  valkey.service        (thêm valkey-sentinel.service)
config   /etc/valkey/valkey.conf
CLI      valkey-cli 8.0.11     ← KHÔNG có redis-cli
```

Protocol tương thích Redis nên client Go dùng bình thường. Chỉ lưu ý: code nào gọi binary `redis-cli` phải đổi.

## 10. nftables

```
nft 1.1.5, nft -j hoạt động
service  nftables.service (disabled mặc định)
config   /etc/sysconfig/nftables.conf
         /etc/nftables/main.nft, /etc/nftables/nat.nft
```

Ruleset hiện tại rỗng. **Không có firewalld, không có iptables** trên máy → không phải gỡ gì, chỉ cần cài `nftables` và nạp ruleset của mình.

`nft -j list ruleset` trả JSON hợp lệ → đọc trạng thái bằng JSON, không parse text. Đúng như plan.

## 11. SELinux — Disabled sẵn

```
getenforce → Disabled
/etc/selinux/config → SELINUX=disabled
semanage, audit2allow: THIẾU (nằm trong policycoreutils-python-utils)
restorecon, setsebool, semodule: có
```

Image của provider tắt sẵn SELinux ở mức boot. Bật lại **không phải chỉ đổi config** — cần `SELINUX=permissive` + `touch /.autorelabel` + **reboot** để relabel toàn hệ thống.

Đề xuất: Stage A chấp nhận SELinux disabled (đúng thực tế đa số server hosting), nhưng **installer phải phát hiện và ghi rõ vào báo cáo cài đặt**, đồng thời không được viết code ngầm phụ thuộc SELinux đang tắt. Việc bật + đóng policy module `opanel.pp` đưa vào backlog sau Stage B.

## 12. User / group / quota

- **Không có `www-data`, `apache`, `nginx`.** Chỉ có `nobody` (65534). → Tạo `opanel-web` như plan.
- `UID_MIN=1000`, `GID_MIN=1000` (CageFS Stage B sẽ dùng số này).
- `/sbin/nologin` và `/usr/sbin/nologin` cùng tồn tại, cùng inode.
- OLS tạo user `lsadm` (uid 994), thuộc nhóm `nobody`.

**⚠ Vấn đề disk quota:** `/` là **xfs mount với `noquota`**, và **không có phân vùng `/home` riêng** — cả hệ thống nằm trên `/dev/sda4`.

XFS không bật được quota bằng remount; với root filesystem phải thêm `rootflags=uquota,gquota` vào kernel cmdline rồi **reboot**. Ba lựa chọn:

1. Kế thừa cách v1: quota mềm ở tầng ứng dụng (đếm dung lượng, chặn khi vượt). Không cần đụng boot.
2. Thêm `rootflags=uquota` vào GRUB + reboot lúc cài → quota cứng ở tầng kernel.
3. Stage B: LVE của CloudLinux có quota riêng.

Đề xuất Stage A dùng (1), giữ nguyên hành vi v1, và ghi rõ đây là quota mềm.

## 13. Mạng

- IPv4 duy nhất `160.236.192.84/24` trên `eth0`.
- **Không có IPv6 public** (chỉ link-local `fe80::`). Firewall vẫn phải sinh rule v6 nhưng không test được v6 thật trên máy này.

---

## Việc phát sinh cho các phase sau

| # | Việc | Phase |
|---|---|---|
| 1 | `phpmgr.SupportedVersions` cho lsphp = 8.1–8.5. Quyết định sản phẩm về khách PHP 7.4 | Phase 2 |
| 2 | Cổng validate OLS: `rc != 2 && !contains("[ERROR]")` | Phase 2 |
| 3 | Golden-file test là lưới an toàn duy nhất cho directive sai tên | Phase 2 |
| 4 | Sau khi ghi config OLS phải `chown lsadm:nobody` | Phase 2 |
| 5 | Đổi OLS từ 8088/7080 sang 80/443 trong bước cài | Phase 2 |
| 6 | Match block SFTP append cuối `sshd_config`, không để trong `sshd_config.d/` | Phase 2 |
| 7 | Dùng `MariaDB-server` (viết hoa), socket `/var/lib/mysql/mysql.sock` | Phase 3 |
| 8 | `valkey-cli` thay `redis-cli` | Phase 3 |
| 9 | Quyết định quota mềm vs `rootflags=uquota` + reboot | Phase 3 |
| 10 | Installer báo cáo trạng thái SELinux thay vì giả định | Phase 2 |
| 11 | Bỏ toàn bộ code cài ionCube thủ công | Phase 2 |

## 14b. ⚠ Provider có cloud firewall phía trên — chỉ mở 22

Bằng chứng: ở lần đo **đầu tiên**, khi máy chưa cài firewall nào và không có service nào nghe ở 80/443, hai cổng đó vẫn trả `timeout` thay vì `refused`. Không có bộ lọc phía trên thì kernel đã phải trả RST.

Đo lại sau khi dựng nftables (cho phép 22/80/443):

| Cổng | Kết quả từ ngoài | Diễn giải |
|---|---|---|
| 22 | OPEN | ✅ |
| 80, 443 | filtered | ❌ nftables cho qua, nhưng **cloud firewall chặn** |
| 3306, 7080, 8088 | filtered | ✅ đúng ý đồ |

**Việc cần làm trước Phase 2:** mở **80** và **443** trong panel nhà cung cấp, cộng với **cổng panel** (mặc định dự kiến 2222). Không mở thì không test được website nào, và không cấp được SSL Let's Encrypt qua HTTP-01.

Không mở 7080/8088 ra Internet — WebAdmin của OLS truy cập qua SSH tunnel khi cần.

## 15. Firewall đã dựng (2026-09-14)

`/etc/nftables/opanel.nft` — bản nháp đầu của firewall renderer Phase 3:

```
table inet opanel {
    set blocklist4 { type ipv4_addr; flags interval; }
    set blocklist6 { type ipv6_addr; flags interval; }
    chain input {
        type filter hook input priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        iif lo accept
        ip saddr @blocklist4 drop
        ip6 saddr @blocklist6 drop
        ip protocol icmp accept
        ip6 nexthdr ipv6-icmp accept
        tcp dport 22 accept   # ssh
        tcp dport 80 accept   # http
        tcp dport 443 accept  # https
    }
    chain forward { type filter hook forward priority filter; policy drop; }
    chain output  { type filter hook output  priority filter; policy accept; }
}
```

Persist: `/etc/sysconfig/nftables.conf` chứa `include "/etc/nftables/opanel.nft"`, `nftables.service` đã enable.

**Kỹ thuật đáng đưa vào installer Phase 3 — dead-man switch.** Trước khi nạp ruleset:

```bash
systemd-run --on-active=240 --unit=opanel-fw-rollback /usr/sbin/nft flush ruleset
```

Nạp ruleset, mở **kết nối SSH mới** để xác minh (kết nối cũ sống nhờ `ct state established` nên không chứng minh được gì), rồi mới `systemctl stop opanel-fw-rollback.timer`. Nếu ruleset sai và mất SSH, 4 phút sau máy tự mở lại. Không có bước này thì một lỗi trong renderer là mất máy.

## Trạng thái máy sau khi khảo sát

Đã cài: `openlitespeed 1.9.2`, `lsphp81/83/84` + extension, `MariaDB-server 11.8.9` (đang chạy), `valkey 8.0.11` (đang chạy), `nftables 1.1.5` (đang chạy, ruleset đã nạp + persist), `zstd`, `git`, `golang 1.26.7`.
Repo đã thêm: `/etc/yum.repos.d/litespeed.repo`, `/etc/yum.repos.d/mariadb.repo`.
OLS nghe 8088 + 7080 nhưng đã bị nftables chặn từ ngoài.

Còn nợ về bảo mật, xử lý ở Phase 1–2:
- `PermitRootLogin yes` + `PasswordAuthentication yes` đang bật. Đổi sang key-based, tắt password.
- Mật khẩu root hiện tại đã lộ qua kênh chat → cần đổi.
- WebAdmin OLS (7080) chưa đặt mật khẩu riêng (`/usr/local/lsws/admin/misc/admpass.sh`). Đang được che bởi firewall, nhưng nên đặt.
