#!/usr/bin/env bash
# OPanel v2 — Phase 0 bring-up recon cho AlmaLinux 10
#
# Mặc định: CHỈ ĐỌC + thêm repo (không cài package nào).
#   bash phase0-recon.sh
# Thử cài thật để xác minh (OLS, lsphp, MariaDB, valkey, nftables):
#   bash phase0-recon.sh --with-installs
#
# Kết quả ghi ra /root/phase0-report.txt
# Gỡ repo đã thêm: bash phase0-recon.sh --cleanup

set -u
WITH_INSTALLS=0
CLEANUP=0
for a in "$@"; do
  case "$a" in
    --with-installs) WITH_INSTALLS=1 ;;
    --cleanup)       CLEANUP=1 ;;
    *) echo "Tham số lạ: $a"; exit 2 ;;
  esac
done

REPORT=/root/phase0-report.txt
exec > >(tee "$REPORT") 2>&1

sec() { printf '\n\n══════ %s ══════\n' "$1"; }
q()   { printf '\n--- %s\n' "$1"; }
have(){ command -v "$1" >/dev/null 2>&1; }

if [[ $CLEANUP == 1 ]]; then
  rm -fv /etc/yum.repos.d/litespeed.repo /etc/yum.repos.d/mariadb.repo
  echo "Đã gỡ repo do script thêm. Package đã cài (nếu có) không bị đụng tới."
  exit 0
fi

echo "OPanel v2 Phase 0 recon — $(date -Is)"
echo "with-installs=$WITH_INSTALLS"

# ─────────────────────────────────────────────────────────────
sec "1. ĐỊNH DANH HỆ ĐIỀU HÀNH"
cat /etc/os-release
q "kernel / arch"; uname -r; uname -m
q "cldetect (phải KHÔNG có trên AlmaLinux thuần)"
if have cldetect; then cldetect --detect-edition; cldetect --detect-os; else echo "không có — đúng, đây là AlmaLinux thuần"; fi

# ─────────────────────────────────────────────────────────────
sec "2. ẢO HOÁ  ⚠ QUYẾT ĐỊNH STAGE B"
# LVE của CloudLinux KHÔNG chạy trên container (OpenVZ/LXC).
q "systemd-detect-virt"; systemd-detect-virt || true
q "hypervisor flag"; grep -o hypervisor /proc/cpuinfo | head -1 || echo "(không thấy)"
q "hostnamectl"; hostnamectl 2>/dev/null | sed -n '1,14p'
echo
V="$(systemd-detect-virt 2>/dev/null || echo unknown)"
case "$V" in
  kvm|none|microsoft|vmware|xen) echo "✅ $V — LVE/CageFS chạy được" ;;
  openvz|lxc|lxc-libvirt|docker|podman) echo "❌ $V — LVE KHÔNG chạy được. Stage B bất khả thi trên máy này." ;;
  *) echo "⚠ '$V' — cần xác minh thủ công với nhà cung cấp" ;;
esac

# ─────────────────────────────────────────────────────────────
sec "3. TÀI NGUYÊN"
q "CPU"; nproc; grep -m1 'model name' /proc/cpuinfo
q "RAM"; free -h
q "Disk"; df -hT / /home /var 2>/dev/null
q "Rotational (0=SSD)"; lsblk -d -o NAME,SIZE,ROTA,TYPE 2>/dev/null

# ─────────────────────────────────────────────────────────────
sec "4. SELINUX  ⚠ QUYẾT ĐỊNH PHASE 2"
q "getenforce"; getenforce 2>/dev/null || echo "không có"
q "sestatus"; sestatus 2>/dev/null | head -10
q "công cụ policy"
for t in semanage restorecon setsebool audit2allow checkmodule semodule; do
  printf '%-14s %s\n' "$t" "$(command -v $t 2>/dev/null || echo '(thiếu)')"
done
q "package cần cho policy module"
rpm -q policycoreutils-python-utils selinux-policy-devel 2>&1 | sed 's/^/  /'

# ─────────────────────────────────────────────────────────────
sec "5. TRÌNH QUẢN LÝ GÓI  ⚠ dnf5 khác cú pháp dnf4"
q "dnf version"; dnf --version 2>&1 | head -3
q "dnf5?"; if rpm -q dnf5 >/dev/null 2>&1; then echo "✅ dnf5 — LƯU Ý: 'dnf groupinstall' đổi thành 'dnf group install'"; else echo "dnf4"; fi
q "repolist"; dnf -q repolist 2>&1
q "EPEL"; rpm -q epel-release 2>/dev/null || echo "chưa cài (cần cho clamav, certbot nếu dùng)"

# ─────────────────────────────────────────────────────────────
sec "6. GÓI CÓ SẴN TRONG REPO GỐC (chỉ tra, không cài)"
for p in valkey redis nftables iptables iptables-nft ipset firewalld \
         mariadb-server golang nodejs npm dnf-automatic \
         policycoreutils-python-utils zstd tar acl quota chrony; do
  v="$(dnf -q list --available "$p" 2>/dev/null | awk 'NR>1{print $2; exit}')"
  i="$(rpm -q --qf '%{VERSION}-%{RELEASE}' "$p" 2>/dev/null)"
  if   [[ -n "${i:-}" && $i != *"not installed"* ]]; then printf '%-32s ĐÃ CÀI  %s\n' "$p" "$i"
  elif [[ -n "${v:-}" ]];                               then printf '%-32s có      %s\n' "$p" "$v"
  else                                                       printf '%-32s ❌ KHÔNG CÓ trong repo\n' "$p"; fi
done
q "valkey vs redis — EL10 đã thay redis bằng valkey"
dnf -q list --available 'valkey*' 2>/dev/null | head -8
dnf -q list --available 'redis*'  2>/dev/null | head -5 || echo "  (không có redis — đúng như dự kiến)"

# ─────────────────────────────────────────────────────────────
sec "7. REPO LITESPEED  ⚠ QUYẾT ĐỊNH STAGE A"
if [[ ! -f /etc/yum.repos.d/litespeed.repo ]]; then
  echo "Thêm repo LiteSpeed…"
  curl -fsSL --connect-timeout 15 --max-time 120 https://repo.litespeed.sh | bash 2>&1 | tail -5
fi
q "openlitespeed có trên el10?"
dnf -q list --available openlitespeed 2>&1 | head -5
q "các bản lsphp có sẵn (đây là danh sách multi-PHP của Stage A)"
dnf -q list --available 'lsphp*' 2>/dev/null | awk '{print $1}' | grep -oE '^lsphp[0-9]+' | sort -u -V
q "extension của một bản mẫu (lsphp84)"
dnf -q list --available 'lsphp84*' 2>/dev/null | awk 'NR>1{print "  "$1}' | head -40
q "ols-modsecurity?"
dnf -q list --available ols-modsecurity 2>&1 | head -4

# ─────────────────────────────────────────────────────────────
sec "8. REPO MARIADB 11.8 LTS"
if [[ ! -f /etc/yum.repos.d/mariadb.repo ]]; then
  echo "Thêm repo MariaDB 11.8…"
  curl -LsS --connect-timeout 15 --max-time 120 https://r.mariadb.com/downloads/mariadb_repo_setup \
    | bash -s -- --os-type=rhel --os-version=10 --mariadb-server-version=mariadb-11.8 2>&1 | tail -6
fi
q "mariadb-server nào sẽ được cài"
dnf -q list --available mariadb-server 2>&1 | head -5

# ─────────────────────────────────────────────────────────────
sec "9. FIREWALL  ⚠ QUYẾT ĐỊNH PHASE 3"
q "nft"; have nft && nft --version || echo "nftables chưa cài"
q "nft -j (v2 cần JSON output, không parse text)"
if have nft; then nft -j list ruleset 2>&1 | head -c 400; echo; else echo "(bỏ qua)"; fi
q "iptables còn tồn tại trên EL10 không?"
have iptables && { iptables --version; echo "  → vẫn còn (nhưng v2 không dùng)"; } || echo "  ❌ không có — đúng như dự đoán, EL10 bỏ iptables"
have ipset && ipset --version || echo "  ipset: không có"
q "firewalld"
systemctl is-enabled firewalld 2>&1; systemctl is-active firewalld 2>&1
q "ruleset hiện tại"; have nft && nft list ruleset 2>&1 | head -30

# ─────────────────────────────────────────────────────────────
sec "10. DỊCH VỤ CÓ THỂ XUNG ĐỘT"
for u in httpd nginx mysqld mariadb apache2 litespeed lshttpd lsws postfix named; do
  s="$(systemctl is-enabled "$u" 2>/dev/null || echo -)"
  a="$(systemctl is-active  "$u" 2>/dev/null || echo -)"
  [[ $s == - && $a == - ]] || printf '%-12s enabled=%-10s active=%s\n' "$u" "$s" "$a"
done
echo "(trống = sạch, tốt)"
q "cổng đang lắng nghe"; ss -tlnp 2>/dev/null | sed -n '1,20p'

# ─────────────────────────────────────────────────────────────
sec "11. SSHD  ⚠ thứ tự Include quyết định cách ghi cấu hình SFTP"
q "10 dòng đầu sshd_config"; head -12 /etc/ssh/sshd_config
q "có Include không, ở dòng nào"
grep -n '^[[:space:]]*Include' /etc/ssh/sshd_config || echo "  không có Include"
q "/etc/ssh/sshd_config.d/"; ls -la /etc/ssh/sshd_config.d/ 2>/dev/null || echo "  (không có thư mục)"
q "Port đang dùng"; sshd -T 2>/dev/null | grep -E '^(port|subsystem)' || grep -E '^Port' /etc/ssh/sshd_config

# ─────────────────────────────────────────────────────────────
sec "12. NGƯỜI DÙNG / NHÓM"
q "UID_MIN (CageFS Stage B dùng số này)"; grep -E '^UID_MIN|^GID_MIN' /etc/login.defs
q "nologin ở đâu"; ls -l /sbin/nologin /usr/sbin/nologin 2>&1
q "có www-data / apache / nobody không"
for u in www-data apache nginx nobody; do printf '%-10s %s\n' "$u" "$(id "$u" 2>&1 | head -1)"; done
q "quota"; have quota && quota --version 2>&1 | head -2 || echo "  chưa cài"
q "mount options của /home (cần usrquota nếu dùng disk quota)"
findmnt -no SOURCE,TARGET,FSTYPE,OPTIONS /home 2>/dev/null || findmnt -no SOURCE,TARGET,FSTYPE,OPTIONS /

# ─────────────────────────────────────────────────────────────
sec "13. TOOLCHAIN"
for t in go node npm git curl tar zstd openssl python3; do
  printf '%-9s %s\n' "$t" "$(command -v $t >/dev/null 2>&1 && $t --version 2>&1 | head -1 || echo '(thiếu)')"
done
q "nodejs trong repo (cần cho build frontend)"
dnf -q module list nodejs 2>/dev/null | head -12 || dnf -q list --available nodejs 2>/dev/null | head -5

# ─────────────────────────────────────────────────────────────
sec "14. MẠNG"
q "IP public"; curl -fsS --max-time 10 https://ifconfig.me 2>/dev/null; echo
q "interface"; ip -4 -br addr
q "IPv6"; ip -6 -br addr | head -5
q "DNS"; cat /etc/resolv.conf | grep -v '^#'

# ─────────────────────────────────────────────────────────────
if [[ $WITH_INSTALLS == 1 ]]; then
sec "15. CÀI THỬ  ⚠ THAY ĐỔI HỆ THỐNG THẬT"

  q "15.1 OpenLiteSpeed + lsphp84"
  dnf install -y openlitespeed lsphp84 lsphp84-mysqlnd lsphp84-opcache 2>&1 | tail -15
  echo "→ layout:"; ls -d /usr/local/lsws/lsphp*/ 2>/dev/null
  echo "→ ⚠ THƯ MỤC INI SCAN THẬT (quyết định thiết kế phpmgr):"
  /usr/local/lsws/lsphp84/bin/php -i 2>/dev/null | grep -iE 'Scan this dir|Loaded Configuration' || echo "  không chạy được binary"
  echo "→ lsphp binary:"; ls -l /usr/local/lsws/lsphp84/bin/ 2>/dev/null | head
  echo "→ ionCube có sẵn không:"; /usr/local/lsws/lsphp84/bin/php -m 2>/dev/null | grep -i ioncube || echo "  KHÔNG có (phải tự cài — alt-php ở Stage B thì có sẵn)"
  echo "→ service unit:"; systemctl list-unit-files 2>/dev/null | grep -iE 'lsws|lshttpd|litespeed'
  echo "→ có lệnh test config không:"
  /usr/local/lsws/bin/openlitespeed -h 2>&1 | head -20

  q "15.2 MariaDB 11.8"
  dnf install -y mariadb-server 2>&1 | tail -10
  mariadbd --version 2>/dev/null || mysqld --version 2>/dev/null
  echo "→ thư mục config:"; ls -la /etc/my.cnf.d/ 2>/dev/null
  echo "→ socket mặc định:"; mariadbd --help --verbose 2>/dev/null | grep -m1 -E '^socket'

  q "15.3 valkey"
  dnf install -y valkey 2>&1 | tail -6
  systemctl list-unit-files 2>/dev/null | grep -i valkey
  ls -la /etc/valkey/ 2>/dev/null

  q "15.4 nftables"
  dnf install -y nftables 2>&1 | tail -5
  nft --version

  q "15.5 SELinux: label mặc định của webroot"
  install -d /home/_probe/public_html
  ls -Zd /home/_probe /home/_probe/public_html
  restorecon -Rv /home/_probe 2>&1 | head -5
  ls -Zd /home/_probe/public_html
  rm -rf /home/_probe
fi

# ─────────────────────────────────────────────────────────────
sec "KẾT LUẬN CẦN ĐỐI CHIẾU VỚI PLAN"
cat <<'SUM'
Bảy câu hỏi của Phase 0 (docs/PLAN-v2-golang.md §5):
  1. lsphp nào có trên el10, thư mục ini scan thật     → mục 7 và 15.1
  2. MariaDB 11.8 từ repo chính thức                    → mục 8 và 15.2
  3. nftables / nft -j / firewalld / iptables còn không → mục 9
  4. SELinux + label webroot                            → mục 4 và 15.5
  5. valkey service + config path                       → mục 6 và 15.3
  6. Node.js + Go toolchain                             → mục 13
  7. KVM hay container (quyết định Stage B)             → mục 2

Gửi lại file /root/phase0-report.txt
Gỡ repo script đã thêm: bash phase0-recon.sh --cleanup
SUM
echo
echo "Report: $REPORT"
