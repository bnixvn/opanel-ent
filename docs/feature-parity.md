# Đối chiếu tính năng v1 (Python) ↔ v2 (Go)

Cập nhật: 2026-09-14, sau Phase 2.

**Không có tính năng nào bị xoá.** Toàn bộ code v1 vẫn nằm nguyên trong repo
(`backend/`, `frontend/`, `installer/install.sh`…) và chưa bị đụng tới. v2 là
bản viết lại từ đầu, hiện ở **Phase 2 trên tổng số 8 phase** của
`docs/PLAN-v2-golang.md`. Những gì chưa thấy là **chưa viết**, không phải mất.

Giao diện tạm hiện tại chỉ phơi ra những gì đã có, nên nhìn vào dễ tưởng sản
phẩm thụt lùi. Bảng dưới là con số thật.

## Tổng quan

| | Số lượng |
|---|---|
| Tính năng v1 (theo README) | 22 |
| v2 đã xong | **6** |
| v2 làm dở (có code, thiếu API/UI) | **3** |
| v2 chưa bắt đầu | **13** |

## Đã xong

| # | Tính năng | Ghi chú |
|---|---|---|
| 1 | Panel user ↔ Linux/SFTP user, site ở `/home/<user>/<domain>/public_html` | Kèm chroot SFTP, cô lập đã kiểm chứng |
| 2 | Site WordPress/PHP/static với vhost OpenLiteSpeed do panel quản | Renderer có golden test |
| 3 | `.htaccess` đầy đủ (`allowOverride`) | |
| 4 | Multi-PHP: cài/gỡ, chọn version theo từng site | 8.1–8.5 (repo el10 không có 7.4) |
| 5 | Phân quyền Admin / Reseller / End user | |
| 6 | Admin tạo site thay mặt user, mỗi site một chủ | |

Ngoài danh sách v1, v2 có thêm: audit log hai lớp, TLS cho panel, cấp cert
Let's Encrypt in-process, rollback tự động khi apply config hỏng, installer
một lệnh idempotent.

## Làm dở — có code, chưa dùng được

| Tính năng | Đã có | Còn thiếu |
|---|---|---|
| **2FA (Google Authenticator)** | Sinh secret, xác thực TOTP, recovery code dùng một lần, luồng login đã xử lý — có test | **Không có endpoint để bật.** User hiện không tự bật 2FA được |
| **API token (cho WHMCS)** | Cấp/thu hồi/xác thực, scope, hash-only — có test | Không có endpoint quản lý |
| **Tường lửa** | nftables ruleset dựng lúc cài, có dead-man switch, persist qua reboot | **Không có API/UI.** Chưa có quản lý rule, blocklist, mở/đóng port |

## Chưa bắt đầu

| # | Tính năng | Phase dự kiến |
|---|---|---|
| 1 | Dashboard tài nguyên CPU/RAM/disk/network | 4 |
| 2 | WordPress one-click + WP-CLI | 3 |
| 3 | Giới hạn số site + quota dung lượng theo user | 5 |
| 4 | Quản lý database MariaDB + phpMyAdmin SSO | 3 |
| 5 | Auto-tuner MariaDB | 3 |
| 6 | Auto-tuner PHP/LSPHP (OPcache, LSAPI worker) | 3 |
| 7 | **Editor cấu hình PHP theo version** | 3 |
| 8 | SSL Let's Encrypt **cho từng site** | 3 |
| 9 | File manager | 4 |
| 10 | Backup: file + SQL, lịch, restore, tải lên/xuống | 4 |
| 11 | SFTP backup target | 4 |
| 12 | Cron manager | 4 |
| 13 | WAF/ModSecurity + HTTP flood | 4 |
| 14 | Quét mã độc (ClamAV + LMD) | 4 |
| 15 | Terminal | 4 |
| 16 | Import backup DirectAdmin | 6 |
| 17 | Provisioning + module WHMCS | 5 |
| 18 | Quản lý cập nhật OS | 4 |

Ghi chú: cột "Phase" theo `docs/PLAN-v2-golang.md`. WAF hiện chỉ có cột
`waf_enabled` trong DB và cờ trong renderer — chưa có engine nào phía sau.

## Tiến độ so với kế hoạch

Plan ước lượng Stage A (đầy đủ tính năng, chạy AlmaLinux 10) khoảng **11–12
tuần công sức**. Phase 0–2 đã xong, tương đương khoảng **3–4 tuần** trong ước
lượng đó. Đúng tiến độ, không chậm — nhưng cũng có nghĩa là còn khoảng hai
phần ba chặng đường.

## Điều đáng bàn về thứ tự

Thứ tự phase trong plan tối ưu cho "dựng nền chắc trước". Nếu mục tiêu là bán
hosting sớm, thứ tự đó có thể không khớp: một khách shared hosting tối thiểu
cần **database + SSL cho site + backup**, và nhà cung cấp cần **quota + WHMCS**
để thu tiền. Ngược lại terminal, file manager, quét mã độc là thứ bán được mà
chưa cần có ngay từ ngày đầu.
