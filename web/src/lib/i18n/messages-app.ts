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
  "settings.keepkey": {
    en: "Leave blank to keep the current key",
    vi: "Để trống để giữ key hiện tại",
  },
  "settings.llm.title": { en: "LLM provider", vi: "Nhà cung cấp LLM" },
  "settings.llm.configured": {
    en: "Configured — {base_url}, key ••••{last4}, timeout {timeout} s, models {trader_model} / {default_model}, updated {time} by {by}",
    vi: "Đã cấu hình — {base_url}, key ••••{last4}, timeout {timeout} s, model {trader_model} / {default_model}, cập nhật {time} bởi {by}",
  },
  "settings.baseurl": { en: "Base URL", vi: "Base URL" },
  "settings.timeout": { en: "Timeout (seconds)", vi: "Timeout (giây)" },
  "settings.llm.trader_model": {
    en: "Model — trader role",
    vi: "Model — vai trader",
  },
  "settings.llm.default_model": {
    en: "Model — analyst roles",
    vi: "Model — các vai phân tích",
  },
  "settings.llm.model_hint": {
    en: "Any model your provider supports; models outside the price table are metered as estimated $0.",
    vi: "Bất kỳ model nào nhà cung cấp hỗ trợ; model ngoài bảng giá sẽ được ghi nhận chi phí ước tính $0.",
  },

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

  // ---- admin: API tokens ----
  "admin.tokens.title": { en: "API tokens", vi: "API token" },
  "admin.tokens.mint": { en: "Mint token", vi: "Tạo token" },
  "admin.tokens.cancel": { en: "Cancel", vi: "Hủy" },
  "admin.tokens.principal": { en: "Principal", vi: "Chủ thể" },
  "admin.principal.user": { en: "User", vi: "Người dùng" },
  "admin.principal.agent": { en: "Agent", vi: "Agent" },
  "admin.tokens.strategy": { en: "Strategy ID", vi: "ID chiến lược" },
  "admin.tokens.label": { en: "Label", vi: "Nhãn" },
  "admin.tokens.label.ph": {
    en: "what this token is for",
    vi: "token này dùng để làm gì",
  },
  "admin.tokens.tenant.ph": { en: "select a tenant…", vi: "chọn tenant…" },
  "admin.tokens.role.ph": { en: "select a role…", vi: "chọn vai trò…" },
  "admin.tokens.tbl.rolestrategy": {
    en: "Role / Strategy",
    vi: "Vai trò / Chiến lược",
  },
  "admin.tokens.active": { en: "active", vi: "hoạt động" },
  "admin.tokens.revoked": { en: "revoked", vi: "đã thu hồi" },
  "admin.tokens.revoke": { en: "Revoke", vi: "Thu hồi" },
  "admin.tokens.revoke.confirm": {
    en: "Confirm revoke",
    vi: "Xác nhận thu hồi",
  },
  "admin.tokens.warn.once": {
    en: "Copy this token now — it is shown this one time only and can never be retrieved again.",
    vi: "Sao chép token này ngay — token chỉ hiển thị duy nhất lần này và không bao giờ có thể xem lại.",
  },
  "admin.tokens.copy": { en: "Copy", vi: "Sao chép" },
  "admin.tokens.copied": {
    en: "Copied to clipboard.",
    vi: "Đã sao chép vào clipboard.",
  },
  "admin.tokens.none": { en: "No API tokens yet.", vi: "Chưa có API token nào." },
  "admin.tokens.none.hint": {
    en: "Mint a token with the button above.",
    vi: "Tạo token bằng nút phía trên.",
  },
  "admin.tokens.err.label": {
    en: "Label is required.",
    vi: "Bắt buộc nhập nhãn.",
  },
  "admin.tokens.err.tenant": {
    en: "Select a tenant.",
    vi: "Hãy chọn tenant.",
  },
  "admin.tokens.err.role": {
    en: "User tokens require a role.",
    vi: "Token người dùng cần có vai trò.",
  },
  "admin.tokens.err.strategy": {
    en: "Agent tokens require a strategy ID.",
    vi: "Token agent cần có ID chiến lược.",
  },

  // ---- billing ----
  "nav.billing": { en: "Billing", vi: "Thanh toán" },
  "billing.title": { en: "Billing", vi: "Thanh toán" },
  "billing.sub": {
    en: "Monthly LLM-cost invoices and reconciliation runs against client-reported costs.",
    vi: "Hóa đơn chi phí LLM hàng tháng và các lần đối soát với chi phí do client báo cáo.",
  },
  "billing.invoices": { en: "Invoices", vi: "Hóa đơn" },
  "billing.recons": { en: "Reconciliations", vi: "Đối soát" },
  "billing.denied": {
    en: "Billing is restricted to tenant admins, owners, and platform administrators.",
    vi: "Trang thanh toán chỉ dành cho quản trị viên tenant, chủ sở hữu và quản trị viên nền tảng.",
  },
  "billing.empty.invoices": { en: "No invoices yet.", vi: "Chưa có hóa đơn nào." },
  "billing.empty.invoices.hint": {
    en: "Invoices appear here once a billing period is generated.",
    vi: "Hóa đơn sẽ xuất hiện tại đây khi một kỳ thanh toán được tạo.",
  },
  "billing.empty.recons": { en: "No reconciliation runs yet.", vi: "Chưa có lần đối soát nào." },
  "billing.empty.recons.hint": {
    en: "Reconciliation runs appear here after invoices are checked against client-reported costs.",
    vi: "Các lần đối soát sẽ xuất hiện tại đây sau khi hóa đơn được đối chiếu với chi phí do client báo cáo.",
  },
  "billing.tbl.period": { en: "Period", vi: "Kỳ" },
  "billing.tbl.tenant": { en: "Tenant", vi: "Tenant" },
  "billing.tbl.total": { en: "Total (USD)", vi: "Tổng (USD)" },
  "billing.tbl.lines": { en: "Lines", vi: "Số dòng" },
  "billing.tbl.generated": { en: "Generated at", vi: "Tạo lúc" },
  "billing.tbl.details": { en: "Details", vi: "Chi tiết" },
  "billing.tbl.strategy": { en: "Strategy", vi: "Chiến lược" },
  "billing.tbl.model": { en: "Model", vi: "Model" },
  "billing.tbl.entry": { en: "Entry type", vi: "Loại mục" },
  "billing.tbl.origperiod": { en: "Original period", vi: "Kỳ gốc" },
  "billing.tbl.intok": { en: "Input tokens", vi: "Token đầu vào" },
  "billing.tbl.outtok": { en: "Output tokens", vi: "Token đầu ra" },
  "billing.tbl.amount": { en: "Amount (USD)", vi: "Số tiền (USD)" },
  "billing.tbl.status": { en: "Status", vi: "Trạng thái" },
  "billing.tbl.counts": {
    en: "Matched / Discrepancies",
    vi: "Khớp / Chênh lệch",
  },
  "billing.tbl.totals": {
    en: "Invoice / Matched (USD)",
    vi: "Hóa đơn / Khớp (USD)",
  },
  "billing.tbl.runat": { en: "Run at", vi: "Chạy lúc" },
  "billing.tbl.class": { en: "Class", vi: "Phân loại" },
  "billing.tbl.request": { en: "Request", vi: "Yêu cầu" },
  "billing.lines.empty": {
    en: "This invoice has no lines.",
    vi: "Hóa đơn này không có dòng nào.",
  },
  "billing.disc.empty": {
    en: "No discrepancies in this run.",
    vi: "Không có chênh lệch trong lần đối soát này.",
  },
  "billing.bucket.matched": {
    en: "Matched client cost",
    vi: "Chi phí client khớp",
  },
  "billing.bucket.orphan": {
    en: "Orphan client cost",
    vi: "Chi phí client không đối ứng",
  },
  "billing.bucket.estimated": {
    en: "Estimated client cost",
    vi: "Chi phí client ước tính",
  },
  "billing.bucket.unattributed": {
    en: "Unattributed client cost",
    vi: "Chi phí client chưa quy nguồn",
  },
  "billing.ops.title": { en: "Billing operations", vi: "Vận hành thanh toán" },
  "billing.ops.tenant.ph": { en: "Select tenant…", vi: "Chọn tenant…" },
  "billing.ops.hint": {
    en: "Only fully elapsed UTC months can be closed — the current running month is rejected.",
    vi: "Chỉ có thể chốt các tháng UTC đã kết thúc — tháng đang chạy sẽ bị từ chối.",
  },
  "billing.ops.close": { en: "Close period", vi: "Chốt kỳ" },
  "billing.ops.close.pending": { en: "Closing…", vi: "Đang chốt…" },
  "billing.ops.close.done": { en: "Period closed", vi: "Đã chốt kỳ" },
  "billing.ops.recon": { en: "Run reconcile", vi: "Chạy đối soát" },
  "billing.ops.recon.pending": { en: "Reconciling…", vi: "Đang đối soát…" },
  "billing.ops.recon.done": {
    en: "Reconciliation complete",
    vi: "Đối soát hoàn tất",
  },
  "billing.ops.dismiss": { en: "Dismiss", vi: "Đóng" },
} as const satisfies Record<string, Msg>;
