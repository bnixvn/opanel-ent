# Đối chiếu tính năng v1 (Python) ↔ v2 (Go)

Cập nhật: 2026-09-14, sau S6 (backup/restore).

**Không có tính năng nào bị xoá.** Toàn bộ code v1 vẫn nằm nguyên trong repo
(`backend/`, `frontend/`, `installer/install.sh`…) và chưa bị đụng tới. v2 là
bản viết lại từ đầu theo `docs/PLAN-v2-golang.md`. Những gì chưa thấy là **chưa
viết**, không phải mất.

## Tổng quan

Bảng dưới liệt kê 30 hạng mục: 22 tính năng v1 theo README, cộng những thứ v1
không có nhưng bắt buộc phải có mới bán được (quản lý user panel, package,
audit log…).

| | Số lượng |
|---|---|
| v2 đã xong | **14** |
| v2 làm dở (có code, thiếu API/UI) | **2** |
| v2 chưa bắt đầu | **14** |

## Đã xong

| # | Tính năng | Ghi chú |
|---|---|---|
| 1 | Panel user ↔ Linux/SFTP user, site ở `/home/<user>/<domain>/public_html` | Kèm chroot SFTP, cô lập đã kiểm chứng |
| 2 | Site WordPress/PHP/static với vhost OpenLiteSpeed do panel quản | Renderer có golden test |
| 3 | `.htaccess` đầy đủ (`allowOverride`) | |
| 4 | Multi-PHP: cài/gỡ, chọn version theo từng site | 8.1–8.5 (repo el10 không có 7.4) |
| 5 | Phân quyền Admin / Reseller / End user | |
| 6 | Admin tạo site thay mặt user, mỗi site một chủ | |
| 7 | Quản lý database MariaDB — tạo/xoá DB, tài khoản, phân quyền, dung lượng | Tên có tiền tố chủ sở hữu; cô lập đã kiểm chứng |
| 8 | SSL Let's Encrypt cho panel, cấp in-process bằng lego | |
| 9 | **Quản lý user panel** — tạo/xoá/khoá, đổi mật khẩu, đổi mật khẩu SFTP, đổi role | Kèm chặn xoá admin cuối cùng và user còn tài nguyên |
| 10 | **2FA (TOTP)** — bật/tắt từ tài khoản, recovery code dùng một lần | |
| 11 | **Package/quota** — giới hạn số site, số database, dung lượng đĩa theo user | Dung lượng đo bằng `du`, mang tính cảnh báo (xem ghi chú) |
| 12 | **SSL Let's Encrypt cho từng site** — cấp, ép HTTPS, tự gia hạn | Đã kiểm chứng từ internet công cộng |
| 13 | **File manager** — duyệt, sửa, đổi tên, chmod, tải lên/xuống, xoá | Cô lập bằng `os.Root`, kernel chặn cả symlink escape |
| 14 | **Backup/restore** — thủ công + theo lịch, retention, tải lên/xuống archive | File + database trong một archive `.tar.gz` mở được bằng `tar` |

Ngoài danh sách v1, v2 có thêm: audit log hai lớp, TLS cho panel, cấp cert
Let's Encrypt in-process, rollback tự động khi apply config hỏng, installer
một lệnh idempotent.

## Làm dở — có code, chưa dùng được

| Tính năng | Đã có | Còn thiếu |
|---|---|---|
| **API token (cho WHMCS)** | Cấp/thu hồi/xác thực, scope, hash-only — có test | Không có endpoint quản lý |
| **Tường lửa** | nftables ruleset dựng lúc cài, có dead-man switch, persist qua reboot | **Không có API/UI.** Chưa có quản lý rule, blocklist, mở/đóng port |

## Chưa bắt đầu

| # | Tính năng | Phase dự kiến |
|---|---|---|
| 1 | SFTP backup target | S7 |
| 2 | WordPress one-click + WP-CLI | S7 (tiếp theo) |
| 3 | Editor cấu hình PHP theo version | S7 |
| 4 | Auto-tuner PHP/LSPHP (OPcache, LSAPI worker) | S7 |
| 5 | Auto-tuner MariaDB | S7 |
| 6 | phpMyAdmin SSO (database engine đã xong) | S7 |
| 7 | API/UI tường lửa | S8 |
| 8 | WAF/ModSecurity + HTTP flood | S8 |
| 9 | Quét mã độc (ClamAV + LMD) | S8 |
| 10 | Cron manager | S9 |
| 11 | Dashboard tài nguyên CPU/RAM/disk/network | S9 |
| 12 | Terminal | S9 |
| 13 | Quản lý cập nhật OS | S9 |
| 14 | Provisioning + module WHMCS, import backup DirectAdmin | S10 |

Ghi chú: cột "Phase" theo bảng S1–S11 trong `docs/PLAN-v2-golang.md`. WAF hiện
chỉ có cột `waf_enabled` trong DB và cờ trong renderer — chưa có engine nào
phía sau. Quota dung lượng đo bằng `du` vì filesystem của host này gắn
`noquota`; muốn cưỡng chế thật thì phải bật project quota trên XFS và khởi
động lại, việc đó để Stage B cùng với LVE.

## Tiến độ so với kế hoạch

Plan ước lượng Stage A (đầy đủ tính năng, chạy AlmaLinux 10) khoảng **11–12
tuần công sức**. Phần đã xong tương đương khoảng **5–6 tuần** trong ước lượng
đó: còn khoảng một nửa chặng đường, và phần còn lại nặng về WordPress
installer, WHMCS và các module bảo mật.

## Điều đáng bàn về thứ tự

Thứ tự phase gốc trong plan tối ưu cho "dựng nền chắc trước". Thứ tự đang chạy
là thứ tự bán được hàng: database → user → package → SSL từng site → file
manager → backup/restore. Với sáu thứ đó thì đã có thể nhận khách thật: tạo
tài khoản, gán gói, dựng site có HTTPS, khách tự sửa file, và dữ liệu có
đường lùi.

Thứ tiếp theo là **WordPress one-click**, vì phần lớn khách mua hosting là để
chạy WordPress, và ngay sau đó là **tường lửa có API/UI** — hiện ruleset dựng
lúc cài rồi bỏ đó, không quản lý được.
