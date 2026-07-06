// Message catalog for the platform surfaces: /settings and /admin.

import type { Msg } from "./messages";

export const messagesApp = {
  // ---- settings ----
  "settings.title": { en: "Platform settings", vi: "Cài đặt nền tảng" },
  "settings.sub": {
    en: "Exchange and LLM credentials for the platform. Values are write-only — once saved they are never displayed again, only the key’s last 4 characters.",
    vi: "Thông tin xác thực sàn và LLM cho nền tảng. Các giá trị chỉ-ghi — sau khi lưu sẽ không bao giờ hiển thị lại, chỉ hiện 4 ký tự cuối của key.",
  },
  "settings.binance.title": { en: "Binance API keys", vi: "API key Binance" },
  "settings.saved": {
    en: "Saved — the stored values will not be shown again.",
    vi: "Đã lưu — các giá trị đã lưu sẽ không được hiển thị lại.",
  },
  "settings.binance.configured": {
    en: "Configured — env {env}, key ••••{last4}, updated {time} by {by}",
    vi: "Đã cấu hình — env {env}, key ••••{last4}, cập nhật {time} bởi {by}",
  },
  "settings.notconfigured": { en: "Not configured", vi: "Chưa cấu hình" },
  "settings.env": { en: "Environment", vi: "Môi trường" },
  "settings.apikey": { en: "API key", vi: "API key" },
  "settings.apisecret": { en: "API secret", vi: "API secret" },
  "settings.prodwarn": {
    en: "prod keys trade real funds — double-check before saving.",
    vi: "key prod giao dịch bằng tiền thật — kiểm tra kỹ trước khi lưu.",
  },
  "settings.save": { en: "Save", vi: "Lưu" },
  "settings.writeonly": {
    en: "Write-only: values are never displayed again.",
    vi: "Chỉ-ghi: các giá trị không bao giờ được hiển thị lại.",
  },
  "settings.llm.title": { en: "LLM provider", vi: "Nhà cung cấp LLM" },
  "settings.llm.configured": {
    en: "Configured — {base_url}, key ••••{last4}, timeout {timeout} s, updated {time} by {by}",
    vi: "Đã cấu hình — {base_url}, key ••••{last4}, timeout {timeout} s, cập nhật {time} bởi {by}",
  },
  "settings.baseurl": { en: "Base URL", vi: "Base URL" },
  "settings.timeout": { en: "Timeout (seconds)", vi: "Timeout (giây)" },

  // ---- admin ----
  "admin.sub": {
    en: "Platform directory — tenants and users. Read-only aside from tenant creation.",
    vi: "Danh bạ nền tảng — tenant và người dùng. Chỉ đọc, ngoại trừ việc tạo tenant.",
  },
  "admin.tenants": { en: "Tenants", vi: "Tenant" },
  "admin.tenantname.placeholder": { en: "tenant name", vi: "tên tenant" },
  "admin.create": { en: "Create", vi: "Tạo" },
  "admin.notenants": { en: "No tenants yet.", vi: "Chưa có tenant nào." },
  "admin.users": { en: "Users", vi: "Người dùng" },
  "admin.nousers": { en: "No users.", vi: "Không có người dùng." },
  "admin.tbl.role": { en: "Role", vi: "Vai trò" },
  "admin.tbl.status": { en: "Status", vi: "Trạng thái" },
  "admin.platform": { en: "platform", vi: "nền tảng" },
  "admin.disabled": { en: "disabled", vi: "vô hiệu hóa" },
} as const satisfies Record<string, Msg>;
