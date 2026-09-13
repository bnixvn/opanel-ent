# NÃƒÂ¢ng CÃ¡ÂºÂ¥p Panel Update: Progress, Log, Auto Reload

## Summary
- ÃƒÂp dÃ¡Â»Â¥ng cho **opanel-ent panel update only**.
- HiÃ¡Â»Æ’n thÃ¡Â»â€¹ tiÃ¡ÂºÂ¿n trÃƒÂ¬nh update theo **% dÃ¡Â»Â±a trÃƒÂªn phase cÃ¡Â»â€˜ Ã„â€˜Ã¡Â»â€¹nh** cÃ¡Â»Â§a `installer/update.sh`.
- HiÃ¡Â»Æ’n thÃ¡Â»â€¹ log update ngay trong trang Updates, tÃ¡Â»Â± refresh khi update Ã„â€˜ang chÃ¡ÂºÂ¡y.
- Khi update hoÃƒÂ n tÃ¡ÂºÂ¥t vÃƒÂ  API health check OK, frontend tÃ¡Â»Â± reload trang Ã„â€˜Ã¡Â»Æ’ tÃ¡ÂºÂ£i bundle mÃ¡Â»â€ºi.

## Key Changes
- MÃ¡Â»Å¸ rÃ¡Â»â„¢ng `/var/lib/opanel-ent/update-status.json` vÃ¡Â»â€ºi cÃƒÂ¡c field:
  - `progress_percent`: sÃ¡Â»â€˜ 0-100.
  - `progress_phase`: mÃƒÂ£ phase ngÃ¡ÂºÂ¯n nhÃ†Â° `checking`, `fetching`, `syncing`, `backend`, `frontend`, `restarting`, `healthcheck`, `completed`, `failed`.
  - `progress_message`: dÃƒÂ²ng mÃƒÂ´ tÃ¡ÂºÂ£ Ã„â€˜ang lÃƒÂ m gÃƒÂ¬.
  - `last_update_started_at`, `last_update_finished_at`, `last_update_status`, `last_update_ref` giÃ¡Â»Â¯ tÃ†Â°Ã†Â¡ng thÃƒÂ­ch hiÃ¡Â»â€¡n tÃ¡ÂºÂ¡i.
- CÃ¡ÂºÂ­p nhÃ¡ÂºÂ­t `installer/update.sh`:
  - ThÃƒÂªm helper `update_progress <percent> <phase> <message>`.
  - GÃ¡Â»Âi helper tÃ¡ÂºÂ¡i cÃƒÂ¡c mÃ¡Â»â€˜c chÃƒÂ­nh: backup/check 5%, fetch 15%, sync 25%, runtime/helper 40%, backend deps 55%, migrations 65%, frontend build 80%, restart/reload 92%, health check 98%, completed 100%.
  - Khi lÃ¡Â»â€”i, trap `ERR` ghi `failed`, giÃ¡Â»Â¯ phÃ¡ÂºÂ§n trÃ„Æ’m hiÃ¡Â»â€¡n tÃ¡ÂºÂ¡i, thÃƒÂªm message lÃ¡Â»â€”i nÃ¡ÂºÂ¿u cÃƒÂ³.
- CÃ¡ÂºÂ­p nhÃ¡ÂºÂ­t `installer/files/opanel-ent-helper.sh`:
  - `updates-status` trÃ¡ÂºÂ£ thÃƒÂªm log gÃ¡ÂºÂ§n nhÃ¡ÂºÂ¥t cÃ¡Â»Â§a `opanel-ent-panel-update.service`.
  - NÃ¡ÂºÂ¿u khÃƒÂ´ng cÃƒÂ³ systemd journal thÃƒÂ¬ fallback Ã„â€˜Ã¡Â»Âc `/var/log/opanel-ent-panel-update.log`.
- CÃ¡ÂºÂ­p nhÃ¡ÂºÂ­t backend `backend/app/services/updates.py`:
  - `panel_release_status()` Ã„â€˜Ã¡Â»Âc vÃƒÂ  trÃ¡ÂºÂ£ cÃƒÂ¡c field progress mÃ¡Â»â€ºi.
  - `status()` trÃ¡ÂºÂ£ thÃƒÂªm `panel.log` hoÃ¡ÂºÂ·c `panel_update_log` dÃ¡ÂºÂ¡ng text ngÃ¡ÂºÂ¯n, giÃ¡Â»â€ºi hÃ¡ÂºÂ¡n sÃ¡Â»â€˜ dÃƒÂ²ng Ã„â€˜Ã¡Â»Æ’ UI khÃƒÂ´ng quÃƒÂ¡ nÃ¡ÂºÂ·ng.
  - KhÃƒÂ´ng thÃƒÂªm API mÃ¡Â»â€ºi; dÃƒÂ¹ng tiÃ¡ÂºÂ¿p `GET /updates/status`.
- CÃ¡ÂºÂ­p nhÃ¡ÂºÂ­t frontend `frontend/src/App.jsx`:
  - Khi bÃ¡ÂºÂ¥m `Update panel now`, mÃ¡Â»Å¸ log panel, set `panelUpdating=true`, gÃ¡Â»Âi `POST /updates/panel/run`, rÃ¡Â»â€œi polling `/updates/status` mÃ¡Â»â€”i 2 giÃƒÂ¢y.
  - HiÃ¡Â»Æ’n thÃ¡Â»â€¹ progress bar, `%`, phase/message, trÃ¡ÂºÂ¡ng thÃƒÂ¡i running/completed/failed.
  - Log tÃ¡Â»Â± cÃ¡ÂºÂ­p nhÃ¡ÂºÂ­t khi Ã„â€˜ang chÃ¡ÂºÂ¡y, khÃƒÂ´ng dÃƒÂ¹ng loading overlay cho polling.
  - Khi `last_update_status === "completed"` vÃƒÂ  `progress_percent === 100`, Ã„â€˜Ã¡Â»Â£i khoÃ¡ÂºÂ£ng 2 giÃƒÂ¢y rÃ¡Â»â€œi gÃ¡Â»Âi `window.location.reload()`.
  - NÃ¡ÂºÂ¿u failed thÃƒÂ¬ dÃ¡Â»Â«ng polling, giÃ¡Â»Â¯ log vÃƒÂ  hiÃ¡Â»â€¡n lÃ¡Â»â€”i/notice.
- CÃ¡ÂºÂ­p nhÃ¡ÂºÂ­t CSS trong `frontend/src/brand.css`:
  - ThÃƒÂªm style cho progress bar, status row, log panel cao cÃ¡Â»â€˜ Ã„â€˜Ã¡Â»â€¹nh, scroll tÃ¡Â»â€˜t trÃƒÂªn mobile.

## Test Plan
- Backend/script:
  - ChÃ¡ÂºÂ¡y shell syntax check: `bash -n installer/update.sh installer/files/opanel-ent-helper.sh`.
  - Test thÃ¡Â»Â§ cÃƒÂ´ng bÃ¡ÂºÂ±ng state file mÃ¡ÂºÂ«u Ã„â€˜Ã¡Â»Æ’ xÃƒÂ¡c nhÃ¡ÂºÂ­n backend trÃ¡ÂºÂ£ Ã„â€˜Ã¡Â»Â§ `progress_percent`, `progress_phase`, `progress_message`.
- Frontend:
  - ChÃ¡ÂºÂ¡y `npm run build` trong `frontend`.
  - KiÃ¡Â»Æ’m tra UI vÃ¡Â»â€ºi status mÃ¡ÂºÂ«u: idle, updating 55%, completed 100%, failed.
  - XÃƒÂ¡c nhÃ¡ÂºÂ­n polling khÃƒÂ´ng bÃ¡ÂºÂ­t loading global liÃƒÂªn tÃ¡Â»Â¥c.
- End-to-end trÃƒÂªn server:
  - BÃ¡ÂºÂ¥m `Update panel now`.
  - ThÃ¡ÂºÂ¥y progress tÃ„Æ’ng theo phase vÃƒÂ  log cÃ¡ÂºÂ­p nhÃ¡ÂºÂ­t.
  - Khi update xong, trang tÃ¡Â»Â± reload vÃƒÂ  vÃ¡ÂºÂ«n Ã„â€˜Ã„Æ’ng nhÃ¡ÂºÂ­p nÃ¡ÂºÂ¿u session cÃƒÂ²n hÃ¡Â»Â£p lÃ¡Â»â€¡.

## Assumptions
- ChÃ¡Â»â€° nÃƒÂ¢ng cÃ¡ÂºÂ¥p luÃ¡Â»â€œng **Panel update**, khÃƒÂ´ng thay Ã„â€˜Ã¡Â»â€¢i Update OS.
- % lÃƒÂ  progress theo phase, khÃƒÂ´ng phÃ¡ÂºÂ£i byte-level progress thÃ¡ÂºÂ­t tÃ¡Â»Â« git/npm/pip.
- GiÃ¡Â»Â¯ nguyÃƒÂªn endpoint hiÃ¡Â»â€¡n tÃ¡ÂºÂ¡i Ã„â€˜Ã¡Â»Æ’ giÃ¡ÂºÂ£m rÃ¡Â»Â§i ro tÃ†Â°Ã†Â¡ng thÃƒÂ­ch.
- Auto reload chÃ¡Â»â€° chÃ¡ÂºÂ¡y khi status lÃƒÂ  `completed`; nÃ¡ÂºÂ¿u `failed` thÃƒÂ¬ khÃƒÂ´ng reload.
