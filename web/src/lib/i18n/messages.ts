// Core message catalog (en/vi): app chrome, public landing, auth pages,
// dashboard. Settings/admin live in messages-app.ts; strategies/reasoning
// live in messages-ops.ts. Keys are flat dotted strings; interpolation uses
// {name} tokens resolved by useI18n().t.

export type Msg = { en: string; vi: string };

export const messages = {
  // ---- chrome ----
  "nav.group.operations": { en: "Operations", vi: "Vận hành" },
  "nav.dashboard": { en: "Dashboard", vi: "Bảng điều khiển" },
  "nav.strategies": { en: "Strategies", vi: "Chiến lược" },
  "nav.group.audit": { en: "Audit", vi: "Kiểm toán" },
  "nav.reasoning": { en: "Reasoning viewer", vi: "Trình xem suy luận" },
  "nav.group.platform": { en: "Platform", vi: "Nền tảng" },
  "nav.settings": { en: "Settings", vi: "Cài đặt" },
  "nav.admin": { en: "Admin", vi: "Quản trị" },
  "nav.signout": { en: "Sign out", vi: "Đăng xuất" },
  "shell.foot.1": { en: "plane boundary enforced", vi: "ranh giới plane được thực thi" },
  "shell.foot.2": { en: "LLMs never touch orders", vi: "LLM không bao giờ chạm vào lệnh" },
  "prefs.theme.dark": { en: "Dark", vi: "Tối" },
  "prefs.theme.light": { en: "Light", vi: "Sáng" },

  // ---- landing ----
  "landing.signin": { en: "Sign in", vi: "Đăng nhập" },
  "landing.getstarted": { en: "Get started", vi: "Bắt đầu" },
  "landing.badge": { en: "LLM-driven trading, human-governed", vi: "Giao dịch bằng LLM, con người giám sát" },
  "landing.title": { en: "Autonomous trading with deterministic guardrails", vi: "Giao dịch tự động với hàng rào bảo vệ tất định" },
  "landing.sub": {
    en: "AlphaMintX runs LLM strategy agents behind a hard plane boundary: models propose, the deterministic Risk Gate and OMS dispose. Climb the autonomy ladder from advisor to full-auto — with kill-switches at every tier and an append-only audit trail.",
    vi: "AlphaMintX vận hành các agent chiến lược LLM sau một ranh giới plane cứng: mô hình đề xuất, Risk Gate tất định và OMS quyết định. Leo thang tự chủ từ cố vấn đến hoàn toàn tự động — với kill-switch ở mọi tầng và nhật ký kiểm toán chỉ-ghi-thêm.",
  },
  "landing.cta.create": { en: "Create a workspace", vi: "Tạo không gian làm việc" },
  "landing.foot": { en: "plane boundary enforced — LLMs never touch orders", vi: "ranh giới plane được thực thi — LLM không bao giờ chạm vào lệnh" },
  "feature.plane.title": { en: "Plane boundary", vi: "Ranh giới plane" },
  "feature.plane.detail": {
    en: "LLMs never place orders. Models propose; the deterministic Risk Gate and the Go OMS dispose — every order, every time.",
    vi: "LLM không bao giờ đặt lệnh. Mô hình đề xuất; Risk Gate tất định và Go OMS quyết định — mọi lệnh, mọi lúc.",
  },
  "feature.ladder.title": { en: "Autonomy ladder", vi: "Thang tự chủ" },
  "feature.ladder.detail": {
    en: "Per-strategy ladder from advisor to full-auto. Promotion to real money requires a code-enforced paper-gate, not a checkbox.",
    vi: "Thang theo từng chiến lược, từ cố vấn đến hoàn toàn tự động. Lên tiền thật phải qua paper-gate do mã nguồn cưỡng chế, không phải một ô tick.",
  },
  "feature.kill.title": { en: "Kill-switch tiers", vi: "Các tầng kill-switch" },
  "feature.kill.detail": {
    en: "Strategy, tenant, and platform kills cancel entries, preserve protective stops, and never auto-restart.",
    vi: "Kill ở mức chiến lược, tenant và nền tảng: hủy lệnh vào, giữ nguyên stop bảo vệ, không bao giờ tự khởi động lại.",
  },
  "feature.limits.title": { en: "Human risk limits", vi: "Giới hạn rủi ro do con người đặt" },
  "feature.limits.detail": {
    en: "Limits are a hard ceiling set by humans — neither trader users nor AI agents can raise them at runtime.",
    vi: "Giới hạn là trần cứng do con người đặt — cả trader lẫn agent AI đều không thể nâng khi đang chạy.",
  },
  "feature.approvals.title": { en: "Copilot approvals", vi: "Phê duyệt copilot" },
  "feature.approvals.detail": {
    en: "Per-proposal human approval with a hard timeout: no decision means auto-reject, never a silent submit.",
    vi: "Con người phê duyệt từng đề xuất với timeout cứng: không quyết định nghĩa là tự động từ chối, không bao giờ lặng lẽ gửi lệnh.",
  },
  "feature.record.title": { en: "Immutable record", vi: "Hồ sơ bất biến" },
  "feature.record.detail": {
    en: "Append-only track record and full reasoning traces — identical strategy code across backtest, paper, and live.",
    vi: "Track record chỉ-ghi-thêm cùng đầy đủ vết suy luận — mã chiến lược y hệt giữa backtest, paper và live.",
  },

  // ---- auth ----
  "auth.email": { en: "Email", vi: "Email" },
  "auth.password": { en: "Password", vi: "Mật khẩu" },
  "login.title": { en: "Sign in", vi: "Đăng nhập" },
  "login.sub": { en: "Session-based access — your token never reaches the browser.", vi: "Truy cập theo phiên — token của bạn không bao giờ tới trình duyệt." },
  "login.pending": { en: "Signing in…", vi: "Đang đăng nhập…" },
  "login.noaccount": { en: "No account?", vi: "Chưa có tài khoản?" },
  "login.bootstrap": { en: "First-run bootstrap", vi: "Khởi tạo lần đầu" },
  "signup.title": { en: "Create a workspace", vi: "Tạo không gian làm việc" },
  "signup.sub": { en: "A tenant plus its owner account — you can invite the team later.", vi: "Một tenant cùng tài khoản chủ sở hữu — bạn có thể mời đội ngũ sau." },
  "signup.workspace": { en: "Workspace name", vi: "Tên không gian làm việc" },
  "signup.pending": { en: "Creating…", vi: "Đang tạo…" },
  "signup.have": { en: "Already have an account?", vi: "Đã có tài khoản?" },
  "bootstrap.title": { en: "Bootstrap platform admin", vi: "Khởi tạo quản trị viên nền tảng" },
  "bootstrap.sub": { en: "One-time first-run setup — creates the platform admin account.", vi: "Thiết lập lần đầu, chỉ một lần — tạo tài khoản quản trị viên nền tảng." },
  "bootstrap.submit": { en: "Create admin", vi: "Tạo quản trị viên" },
  "bootstrap.pending": { en: "Bootstrapping…", vi: "Đang khởi tạo…" },
  "bootstrap.done": { en: "Already bootstrapped?", vi: "Đã khởi tạo rồi?" },

  // ---- dashboard ----
  "dash.title": { en: "Dashboard", vi: "Bảng điều khiển" },
  "dash.sub": {
    en: "Live control-plane view — strategy fleet, lifecycle states, and safety posture, polled every 10 s.",
    vi: "Góc nhìn control-plane trực tiếp — đội chiến lược, trạng thái vòng đời và tư thế an toàn, cập nhật mỗi 10 giây.",
  },
  "dash.stat.total": { en: "Total strategies", vi: "Tổng chiến lược" },
  "dash.stat.total.meta": { en: "Registered across all tenants.", vi: "Đăng ký trên tất cả tenant." },
  "dash.stat.live": { en: "Live", vi: "Live" },
  "dash.stat.live.meta": { en: "Trading real money (L1–L3)", vi: "Giao dịch tiền thật (L1–L3)" },
  "dash.stat.paper": { en: "Paper", vi: "Paper" },
  "dash.stat.paper.meta": { en: "Simulated fills, no exchange orders", vi: "Khớp lệnh mô phỏng, không có lệnh lên sàn" },
  "dash.stat.attention": { en: "Attention", vi: "Cần chú ý" },
  "dash.stat.attention.meta": { en: "Killed or paused — operator review", vi: "Bị kill hoặc tạm dừng — cần người vận hành xem" },
  "dash.ofpage": { en: ", of current page", vi: ", trong trang hiện tại" },
  "dash.strategies": { en: "Strategies", vi: "Chiến lược" },
  "market.title": { en: "Market — Binance", vi: "Thị trường — Binance" },
  "market.tab.spot": { en: "Spot", vi: "Spot" },
  "market.tab.futures": { en: "Futures (USD-M)", vi: "Futures (USD-M)" },
  "market.high": { en: "H", vi: "Cao" },
  "market.low": { en: "L", vi: "Thấp" },
  "market.vol": { en: "Vol", vi: "KL" },
  "market.unavailable": {
    en: "Market data unavailable (browser cannot reach the public Binance feed).",
    vi: "Không lấy được dữ liệu thị trường (trình duyệt không kết nối được feed công khai của Binance).",
  },
  "market.futures.funding": { en: "Fund", vi: "Funding" },
  "market.futures.next": { en: "Next", vi: "Kế tiếp" },
  "market.futures.oi": { en: "OI", vi: "OI" },
  "market.futures.unavailable": {
    en: "Futures feed unavailable from your network location (Binance regional restriction).",
    vi: "Không lấy được dữ liệu Futures từ vị trí mạng của bạn (Binance hạn chế khu vực).",
  },
  "market.chart.ask": { en: "Ask agent", vi: "Hỏi agent" },
  "market.chart.asking": { en: "Asking agent…", vi: "Đang hỏi agent…" },
  "market.chart.asking.hint": {
    en: "The model is writing a full analysis — usually 30–40 s.",
    vi: "Model đang viết bài phân tích đầy đủ — thường mất 30–40 giây.",
  },
  "market.chart.llm.notconfigured": {
    en: "Configure an LLM provider in Settings first.",
    vi: "Hãy cấu hình nhà cung cấp LLM trong Cài đặt trước.",
  },
  "market.chart.model": { en: "model", vi: "mô hình" },
  "market.ta.rsi.overbought": { en: "Overbought", vi: "Quá mua" },
  "market.ta.rsi.bullish": { en: "Bullish momentum", vi: "Động lượng thiên tăng" },
  "market.ta.rsi.bearish": { en: "Bearish momentum", vi: "Động lượng thiên giảm" },
  "market.ta.rsi.oversold": { en: "Oversold", vi: "Quá bán" },
  "market.ta.macd.above": { en: "MACD above signal", vi: "MACD trên đường tín hiệu" },
  "market.ta.macd.below": { en: "MACD below signal", vi: "MACD dưới đường tín hiệu" },
  "market.ta.macd.rising": { en: "histogram rising", vi: "histogram tăng" },
  "market.ta.macd.falling": { en: "histogram falling", vi: "histogram giảm" },
  "market.ta.ma.up": {
    en: "Uptrend (close above SMA25 & SMA99)",
    vi: "Xu hướng tăng (giá trên SMA25 & SMA99)",
  },
  "market.ta.ma.down": {
    en: "Downtrend (close below SMA25 & SMA99)",
    vi: "Xu hướng giảm (giá dưới SMA25 & SMA99)",
  },
  "market.ta.ma.side": {
    en: "Sideways (mixed vs SMA25/SMA99)",
    vi: "Đi ngang (trái chiều so với SMA25/SMA99)",
  },
  "market.ta.ma.golden": { en: "golden cross SMA7/SMA25", vi: "giao cắt vàng SMA7/SMA25" },
  "market.ta.ma.death": { en: "death cross SMA7/SMA25", vi: "giao cắt tử thần SMA7/SMA25" },
  "market.ta.boll.upper": {
    en: "Price stretched near the upper band",
    vi: "Giá căng sát dải trên",
  },
  "market.ta.boll.lower": {
    en: "Price near the lower band (oversold zone)",
    vi: "Giá sát dải dưới (vùng quá bán)",
  },
  "market.ta.boll.mid": { en: "Price inside the bands (neutral)", vi: "Giá trong dải (trung tính)" },
  "market.ta.verdict": { en: "Verdict", vi: "Kết luận" },
  "market.ta.verdict.strongbuy": { en: "Strong Buy", vi: "Mua mạnh" },
  "market.ta.verdict.buy": { en: "Buy", vi: "Mua" },
  "market.ta.verdict.neutral": { en: "Neutral", vi: "Trung tính" },
  "market.ta.verdict.sell": { en: "Sell", vi: "Bán" },
  "market.ta.verdict.strongsell": { en: "Strong Sell", vi: "Bán mạnh" },
  "market.ta.disclaimer": {
    en: "Automated technical readout — not financial advice.",
    vi: "Nhận định kỹ thuật tự động — không phải khuyến nghị đầu tư.",
  },
  "dash.empty": { en: "No strategies yet.", vi: "Chưa có chiến lược nào." },
  "tbl.name": { en: "Name", vi: "Tên" },
  "tbl.state": { en: "State", vi: "Trạng thái" },
  "tbl.tenant": { en: "Tenant", vi: "Tenant" },
  "tbl.id": { en: "ID", vi: "ID" },
  "tbl.created": { en: "Created", vi: "Tạo lúc" },
  "tbl.updated": { en: "Updated", vi: "Cập nhật" },
  "dash.invariants": { en: "Safety invariants", vi: "Bất biến an toàn" },
  "dash.ladder": { en: "Autonomy ladder", vi: "Thang tự chủ" },
  "inv.1": {
    en: "LLMs never place orders directly. Only the Go OMS talks to exchanges; every order passes the deterministic Risk Gate first.",
    vi: "LLM không bao giờ đặt lệnh trực tiếp. Chỉ Go OMS nói chuyện với sàn; mọi lệnh đều qua Risk Gate tất định trước.",
  },
  "inv.2": {
    en: "SL/TP live on the exchange, not in slow LLM loops; no open position without an exchange-resident stop-loss while require_stop_loss=true.",
    vi: "SL/TP nằm trên sàn, không nằm trong vòng lặp LLM chậm chạp; không có vị thế mở nào thiếu stop-loss trên sàn khi require_stop_loss=true.",
  },
  "inv.3": {
    en: "Autonomy ladder per strategy (L0–L3); promotion to real money requires a code-enforced paper-gate.",
    vi: "Thang tự chủ theo từng chiến lược (L0–L3); lên tiền thật phải qua paper-gate do mã nguồn cưỡng chế.",
  },
  "inv.4": {
    en: "Kill-switch at 3 tiers (strategy / tenant / platform): cancel ENTRY orders, preserve protective stops, no auto-restart. Circuit breaker on daily loss demotes to L0 for the UTC day.",
    vi: "Kill-switch 3 tầng (chiến lược / tenant / nền tảng): hủy lệnh ENTRY, giữ nguyên stop bảo vệ, không tự khởi động lại. Circuit breaker theo lỗ ngày hạ xuống L0 trong ngày UTC.",
  },
  "inv.5": {
    en: "Risk limits are set by humans (Admin) — a hard ceiling neither Trader users nor AI agents can raise.",
    vi: "Giới hạn rủi ro do con người (Admin) đặt — trần cứng mà cả trader lẫn agent AI đều không thể nâng.",
  },
  "inv.6": {
    en: "Exchange API keys are write-only after save (field-level encryption); trade-only, never withdrawal-enabled.",
    vi: "API key sàn chỉ-ghi sau khi lưu (mã hóa mức trường); chỉ được giao dịch, không bao giờ bật quyền rút tiền.",
  },
  "inv.7": {
    en: "Track record is immutable/append-only; backtests free of lookahead bias; strategy code identical across backtest / paper / live.",
    vi: "Track record bất biến/chỉ-ghi-thêm; backtest không có lookahead bias; mã chiến lược y hệt giữa backtest / paper / live.",
  },
  "ladder.l0.name": { en: "Advisor", vi: "Cố vấn" },
  "ladder.l0.detail": {
    en: "Proposals persisted and shown only; no OMS submission.",
    vi: "Đề xuất chỉ được lưu và hiển thị; không gửi tới OMS.",
  },
  "ladder.l1.name": { en: "Copilot", vi: "Copilot" },
  "ladder.l1.detail": {
    en: "OMS submits only after per-proposal human approval; no decision within the timeout (default 600 s) ⇒ auto-reject.",
    vi: "OMS chỉ gửi sau khi con người phê duyệt từng đề xuất; không quyết định trong timeout (mặc định 600 s) ⇒ tự động từ chối.",
  },
  "ladder.l2.name": { en: "Semi-auto", vi: "Bán tự động" },
  "ladder.l2.detail": {
    en: "OMS submits automatically within the L2 envelope; above-envelope proposals escalate through the L1 approve flow.",
    vi: "OMS tự gửi trong phạm vi L2; đề xuất vượt phạm vi được đẩy qua luồng phê duyệt L1.",
  },
  "ladder.l3.name": { en: "Full-auto", vi: "Hoàn toàn tự động" },
  "ladder.l3.detail": {
    en: "OMS submits any gate-approved proposal; kill-switch and risk limits still apply.",
    vi: "OMS gửi mọi đề xuất được gate duyệt; kill-switch và giới hạn rủi ro vẫn áp dụng.",
  },

  // ---- a11y-only labels (shared pager) ----
  "ui.pager.prev.label": { en: "Previous page", vi: "Trang trước" },
  "ui.pager.next.label": { en: "Next page", vi: "Trang sau" },
} as const satisfies Record<string, Msg>;
