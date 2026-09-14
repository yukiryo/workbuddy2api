// WorkBuddy WebUI 前端核心逻辑控制层

class WorkBuddyApp {
  constructor() {
    const isBrowserHosted = typeof window !== 'undefined' && window.location.protocol.startsWith('http');
    const autoOrigin = isBrowserHosted ? window.location.origin : 'http://localhost:7863';
    this.defaultGateway = autoOrigin;
    this.gatewayUrl = localStorage.getItem('wb_gateway_url') || this.defaultGateway;
    this.apiKey = localStorage.getItem('wb_api_key') || '';
    this.currentTab = 'dashboard';
    
    // 缓存数据
    this.cachedStatus = null;
    this.cachedModels = [];
    this.cachedUsage = null;
    this.cachedAccountStatus = null;   // 账号深度状态（配额/签到/模型）
    this.cachedCredentials = null;     // 凭证列表
    this.oauthState = null;            // 进行中的 OAuth 会话
    // 模型广场排序方式：default（官方顺序）/ credits（积分倍率升序）
    this.modelSort = localStorage.getItem('wb_model_sort') || 'default';
    // 用量明细分页状态
    this.usageRecLimit = 200;
    this.usageRecOffset = 0;
    this.usageRecTotal = 0;
    this.usageRange = '24h';
    this.isFetching = false;
    this.chatHistory = [];
    this.isChatStreaming = false;
    this.redirectingToLogin = false;

    this.init();
  }

  init() {
    // 规范化 URL（去除末尾斜杠）
    this.gatewayUrl = this.gatewayUrl.replace(/\/+$/, '');
    
    // 初始化界面表单值
    const inputUrl = document.getElementById('setting-gateway-url');
    if (inputUrl) inputUrl.value = this.gatewayUrl;
    const inputKey = document.getElementById('setting-api-key');
    if (inputKey) inputKey.value = this.apiKey;
    this.updateEndpointLabels();

    // 绑定导航与哈希路由
    this.initNavigation();
    
    // 渲染图标
    if (window.lucide) {
      lucide.createIcons();
    }

    // 自动尝试从网关拉取当前生效的 API Key
    this.fetchRouterKey(true);

    // 查询登录态（决定是否显示“退出登录”按钮）
    this.checkSession();

    // 初始化轮询与首次拉取
    this.refreshAll();
    setInterval(() => this.healthCheck(), 10000);
    setInterval(() => this.fetchStatus(), 15000);

    // 监听回车发送测试消息
    const chatInput = document.getElementById('chat-input');
    if (chatInput) {
      chatInput.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && !e.shiftKey) {
          e.preventDefault();
          this.sendChatMessage(e);
        }
      });
    }
  }

  updateEndpointLabels() {
    const v1Url = `${this.gatewayUrl}/v1`;
    const label = document.getElementById('gateway-endpoint-label');
    if (label) label.textContent = this.gatewayUrl;
    
    const dashBaseUrl = document.getElementById('dash-base-url');
    if (dashBaseUrl) dashBaseUrl.value = v1Url;
    
    const dashKey = document.getElementById('dash-api-key');
    if (dashKey) dashKey.value = this.apiKey || 'sk-none (未设密钥)';

    document.querySelectorAll('.guide-url').forEach(el => el.textContent = this.gatewayUrl);
    document.querySelectorAll('.guide-v1-url').forEach(el => el.textContent = v1Url);
    document.querySelectorAll('.guide-key').forEach(el => el.textContent = this.apiKey || 'sk-任意填');
  }

  initNavigation() {
    const handleHash = () => {
      const hash = window.location.hash.replace('#', '') || 'dashboard';
      this.switchTab(hash, false);
    };

    window.addEventListener('hashchange', handleHash);
    handleHash();

    document.querySelectorAll('.nav-item').forEach(item => {
      item.addEventListener('click', (e) => {
        e.preventDefault();
        const tab = item.getAttribute('data-tab');
        window.location.hash = tab;
        this.switchTab(tab);
        this.closeSidebar(); // 移动端选完即收起抽屉
      });
    });

    // 窄屏横竖屏切换 / 拉伸窗口时自动收起抽屉，避免留下半开的侧边栏
    window.addEventListener('resize', () => {
      if (window.innerWidth >= 1024) this.closeSidebar(true);
    });
  }

  // ===== 移动端抽屉侧边栏 =====

  openSidebar() {
    const sb = document.getElementById('sidebar');
    const bd = document.getElementById('sidebar-backdrop');
    if (sb) sb.classList.remove('-translate-x-full');
    if (bd) bd.classList.remove('hidden');
    // 抽屉打开时锁住背景滚动，避免误滑
    document.body.classList.add('overflow-hidden');
  }

  closeSidebar(force = false) {
    const sb = document.getElementById('sidebar');
    const bd = document.getElementById('sidebar-backdrop');
    if (sb) sb.classList.add('-translate-x-full');
    if (bd) bd.classList.add('hidden');
    // 大屏下侧边栏是常驻的，不能锁滚动
    if (force || window.innerWidth < 1024) {
      document.body.classList.remove('overflow-hidden');
    }
  }

  switchTab(tabName, updateHash = true) {
    this.currentTab = tabName;
    if (updateHash) {
      window.location.hash = tabName;
    }

    // 切换侧边栏高亮
    document.querySelectorAll('.nav-item').forEach(item => {
      if (item.getAttribute('data-tab') === tabName) {
        item.classList.add('active');
      } else {
        item.classList.remove('active');
      }
    });

    // 切换内容区域
    document.querySelectorAll('.tab-pane').forEach(pane => {
      pane.classList.add('hidden');
    });
    const targetPane = document.getElementById(`tab-${tabName}`);
    if (targetPane) {
      targetPane.classList.remove('hidden');
    }

    // 更新顶部标题
    const titles = {
      dashboard: ['概览仪表盘', '实时监控网关健康状态与账号调度池'],
      accounts: ['账号状态监测', '配额余量、签到状态与可用模型（点击刷新才请求上游）'],
      credentials: ['凭证管理', '添加 / 删除 CodeBuddy 账号凭证，改动立即热生效'],
      models: ['模型广场', '支持的所有 OpenAI 兼容模型列表'],
      playground: ['API 测试沙盒', '在线体验流式对话，实时监测网关链路延迟'],
      usage: ['Token 用量统计', '多维度监控 Token 消耗与请求分布趋势'],
      guides: ['客户端接入指南', 'Cherry Studio、Chatbox 等主流客户端一键接入'],
      settings: ['连接设置', '配置路由器网关地址与访问认证令牌']
    };

    if (titles[tabName]) {
      document.getElementById('page-title').textContent = titles[tabName][0];
      document.getElementById('page-desc').textContent = titles[tabName][1];
    }

    // 按需加载：只在首次进入时拉数据，避免每次切 tab 都打上游
    if (tabName === 'usage') {
      this.fetchUsage();
    }
    if (tabName === 'accounts') {
      this.fetchStatus();
      // 首次进入才自动查一次深度状态（之后由用户手动刷新，避免频繁请求上游）
      if (!this.cachedAccountStatus) {
        this.fetchAccountStatusAll(false);
      } else {
        this.renderAccountStatusGrid();
      }
    }
    if (tabName === 'credentials') {
      this.fetchCredentials();
    }

    if (window.lucide) {
      lucide.createIcons();
    }
  }

  // 获取实际请求 URL（同源或当前端口即 7863 时优先走相对路径，避免跨域与 IP 漂移）
  getEffectiveUrl(endpoint) {
    if (window.location.protocol.startsWith('http')) {
      if (this.gatewayUrl === window.location.origin || window.location.port === '7863') {
        return endpoint;
      }
      if (window.location.port === '8080' || window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1') {
        return `/proxy${endpoint}`;
      }
    }
    return `${this.gatewayUrl}${endpoint}`;
  }

  // 通用带鉴权的 Fetch 请求
  async apiRequest(endpoint, options = {}) {
    const url = this.getEffectiveUrl(endpoint);
    const headers = { ...options.headers };
    if (this.apiKey) {
      headers['Authorization'] = `Bearer ${this.apiKey}`;
    }

    const res = await fetch(url, {
      ...options,
      headers
    });

    // 登录态失效：统一跳回登录页，避免各处重复处理
    if (res.status === 401 && url.startsWith(window.location.origin)) {
      this.redirectToLogin();
    }
    return res;
  }

  // 跳转登录页（带防重复触发）
  redirectToLogin() {
    if (this.redirectingToLogin) return;
    this.redirectingToLogin = true;
    window.location.replace('/login.html');
  }

  // 退出登录
  async logout() {
    try {
      await fetch('/api/logout', { method: 'POST' });
    } catch (_) {
      // 网络异常也照样回登录页，避免用户卡在无响应的控制台
    }
    this.redirectToLogin();
  }

  // 查询登录态：决定是否显示退出按钮（未启用登录时隐藏）
  async checkSession() {
    try {
      const res = await fetch('/api/session', { cache: 'no-store' });
      if (!res.ok) return;
      const data = await res.json();
      const btn = document.getElementById('btn-logout');
      if (btn && data.login_required && data.logged_in) {
        btn.classList.remove('hidden');
      }
    } catch (_) {
      // 静默失败：不影响控制台主流程
    }
  }

  // 刷新所有数据
  async refreshAll() {
    const icon = document.getElementById('refresh-icon');
    if (icon) icon.classList.add('animate-spin');

    try {
      await Promise.allSettled([
        this.healthCheck(),
        this.fetchStatus(),
        this.fetchModels(),
        this.fetchUsage()
      ]);
      this.showToast('数据已刷新同步', 'success');
    } finally {
      if (icon) icon.classList.remove('animate-spin');
    }
  }

  // 1. 健康检查与延迟测试
  async healthCheck() {
    const dot = document.getElementById('gateway-status-dot');
    const text = document.getElementById('gateway-status-text');
    const pingEl = document.getElementById('gateway-ping');
    const banner = document.getElementById('conn-banner');

    const startTime = performance.now();
    try {
      const url = this.getEffectiveUrl('/healthz');
      const res = await fetch(url, { cache: 'no-store' });
      const latency = Math.round(performance.now() - startTime);

      if (res.ok) {
        const data = await res.json();
        dot.className = 'w-2 h-2 rounded-full dot-healthy';
        text.textContent = '网关运行中';
        pingEl.textContent = `${latency} ms`;
        banner.classList.add('hidden');

        document.getElementById('stat-gateway-service').textContent = data.service || 'workbuddy';
      } else {
        throw new Error(`HTTP ${res.status}`);
      }
    } catch (err) {
      dot.className = 'w-2 h-2 rounded-full dot-error';
      text.textContent = '连接不可达';
      pingEl.textContent = '-- ms';
      banner.classList.remove('hidden');
      document.getElementById('conn-banner-text').textContent = 
        `无法连接到网关 ${this.gatewayUrl}，请检查路由器网络是否通畅或是否存在跨域拦截。`;
    }
  }

  // 2. 拉取账号池状态
  async fetchStatus() {
    try {
      const res = await this.apiRequest('/status');
      if (!res.ok) {
        if (res.status === 401) {
          this.showToast('需要配置 API Key 才能获取完整状态', 'warning');
        }
        return;
      }
      const data = await res.json();
      this.cachedStatus = data;
      this.renderStatus(data);
      // 账号大厅若已打开，同步重绘（合并池状态与新数据）
      if (this.currentTab === 'accounts' && this.cachedAccountStatus) {
        this.renderAccountStatusGrid();
      }
    } catch (err) {
      console.warn('拉取 /status 失败:', err);
    }
  }

  // realm 域统计（/status 的 realm_totals）：只在存在 global 账号时才占版面，
  // 纯 CN 部署下不显示，避免给单域用户增加噪音。
  renderRealmTotals(totals) {
    const box = document.getElementById('realm-totals-box');
    if (!box) return;
    if (!totals || typeof totals !== 'object') { box.classList.add('hidden'); return; }

    const cn = totals.cn || {};
    const gl = totals.global || {};
    const hasGlobal = (gl.total || 0) > 0;

    if (!hasGlobal) { box.classList.add('hidden'); return; }

    const cell = (label, t, color) => `
      <div class="p-3 rounded-xl bg-slate-50 dark:bg-slate-900/50 border border-slate-100 dark:border-slate-800">
        <div class="flex items-center justify-between mb-2">
          <span class="px-1.5 py-0.5 rounded text-[9px] font-semibold ${color}">${label}</span>
          <span class="text-[10px] text-slate-400 font-mono">${t.total || 0} 个</span>
        </div>
        <div class="grid grid-cols-3 gap-1 text-center">
          <div><div class="text-[9px] text-slate-400">健康</div><div class="text-xs font-mono font-semibold text-emerald-500">${t.healthy || 0}</div></div>
          <div><div class="text-[9px] text-slate-400">冷却</div><div class="text-xs font-mono font-semibold text-amber-500">${t.cooling || 0}</div></div>
          <div><div class="text-[9px] text-slate-400">停用</div><div class="text-xs font-mono font-semibold text-rose-500">${t.disabled || 0}</div></div>
        </div>
      </div>`;

    box.innerHTML = `
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
        ${cell('CN', cn, 'bg-sky-500/10 text-sky-500')}
        ${cell('GLOBAL', gl, 'bg-violet-500/10 text-violet-500')}
      </div>`;
    box.classList.remove('hidden');
  }

  renderStatus(data) {
    // 更新总览数据
    const total = data.total || 0;
    const healthy = data.healthy || 0;
    const cooling = data.cooling || 0;
    const disabled = data.disabled || 0;

    document.getElementById('stat-total-accounts').textContent = total;
    document.getElementById('stat-healthy-accounts').textContent = healthy;
    document.getElementById('stat-cooling-accounts').textContent = cooling;
    document.getElementById('stat-disabled-accounts').textContent = disabled;
    document.getElementById('nav-accounts-count').textContent = total;

    // 双域统计（上游 realm 能力：cn / global 分池）
    this.renderRealmTotals(data.realm_totals);

    // 其余运行态指标（此前后端已返回但前端未展示）
    const set = (id, v) => { const el = document.getElementById(id); if (el) el.textContent = v; };
    set('stat-sticky-sessions', data.sticky_sessions ?? 0);
    set('stat-inflight-full', data.in_flight_full ?? 0);

    // 渲染仪表盘账号小预览
    const previewContainer = document.getElementById('dash-accounts-preview');
    const accounts = data.accounts || [];

    if (accounts.length === 0) {
      previewContainer.innerHTML = `
        <div class="text-center py-8 rounded-xl bg-slate-50 dark:bg-slate-900/40 border border-dashed border-slate-200 dark:border-slate-800 text-xs text-slate-400 space-y-2">
          <p>暂无加载的 CodeBuddy 凭证</p>
          <p class="text-[11px] text-slate-500">可在路由器内置闪存的 auths/ 目录下存放凭证文件自动加载</p>
        </div>
      `;
    } else {
      previewContainer.innerHTML = accounts.slice(0, 4).map(acc => this.generateAccountRowHtml(acc)).join('');
    }

    // 账号大厅的卡片由 renderAccountStatusGrid 统一渲染（需合并池状态与深度状态），
    // 此处不再直接写 accounts-grid，避免两条渲染路径互相覆盖。
    if (this.currentTab === 'accounts') {
      this.renderAccountStatusGrid();
    }

    if (window.lucide) lucide.createIcons();
  }

  // ===== 账号深度状态（配额 / 签到 / 可用模型）=====

  // 刷新全部账号的深度状态（真实请求上游）
  async refreshAccountStatus(force = false) {
    const btn = document.getElementById('btn-accounts-refresh');
    const icon = document.getElementById('accounts-refresh-icon');
    if (btn) btn.disabled = true;
    if (icon) icon.classList.add('animate-spin');
    try {
      await this.fetchAccountStatusAll(force);
    } finally {
      if (btn) btn.disabled = false;
      if (icon) icon.classList.remove('animate-spin');
    }
  }

  async fetchAccountStatusAll(force = false) {
    const grid = document.getElementById('accounts-grid');
    if (grid && force) {
      grid.innerHTML = `<div class="col-span-full text-center py-12 text-slate-400 text-xs">
        <i data-lucide="loader-2" class="w-5 h-5 mx-auto mb-2 animate-spin"></i>
        正在查询上游账号状态（配额 / 签到 / 模型），首次可能需要几秒…
      </div>`;
      if (window.lucide) lucide.createIcons();
    }

    try {
      const res = await this.apiRequest(`/api/accounts/status${force ? '?refresh=1' : ''}`);
      const data = await res.json();
      if (!res.ok || !data.success) throw new Error(data.message || `HTTP ${res.status}`);

      this.cachedAccountStatus = data.accounts || [];
      // 把深度状态与池状态合并渲染（池状态字段来自 /status，此处补上配额等）
      this.renderAccountStatusGrid();

      const el = document.getElementById('accounts-updated');
      if (el) el.textContent = `更新于 ${new Date().toLocaleTimeString('zh-CN')}`;
    } catch (err) {
      if (grid) {
        grid.innerHTML = `<div class="col-span-full text-center py-12 text-rose-500 text-xs">
          查询失败: ${this.escapeHtml(err.message)}
        </div>`;
      }
      this.showToast('账号状态查询失败: ' + err.message, 'error');
    }
  }

  // 查询单个账号详情
  async loadAccountStatus(uid) {
    if (!uid) return;
    this.showToast('正在查询该账号详情…', 'info');
    try {
      const res = await this.apiRequest(`/api/accounts/status?uid=${encodeURIComponent(uid)}&refresh=1`);
      const data = await res.json();
      if (!res.ok || !data.success) throw new Error(data.message || `HTTP ${res.status}`);
      const st = (data.accounts || [])[0];
      if (!st) throw new Error('未返回数据');
      // 合并进缓存后重绘
      const list = this.cachedAccountStatus || [];
      const idx = list.findIndex(a => a.uid === uid);
      if (idx >= 0) list[idx] = st; else list.push(st);
      this.cachedAccountStatus = list;
      this.renderAccountStatusGrid();
      this.showToast('详情已更新', 'success');
    } catch (err) {
      this.showToast('查询失败: ' + err.message, 'error');
    }
  }

  // 渲染账号状态卡片（池状态 + 深度状态合并）
  renderAccountStatusGrid() {
    const grid = document.getElementById('accounts-grid');
    if (!grid) return;

    const poolAccounts = (this.cachedStatus && this.cachedStatus.accounts) || [];
    const deepList = this.cachedAccountStatus || [];

    if (poolAccounts.length === 0) {
      grid.innerHTML = `<div class="col-span-full text-center py-12 text-slate-400 text-xs">暂无账号信息</div>`;
      if (window.lucide) lucide.createIcons();
      return;
    }

    grid.innerHTML = poolAccounts.map(acc => {
      const deep = deepList.find(d => d.uid === acc.uid) || {};
      return this.generateStatusCardHtml(acc, deep);
    }).join('');

    if (window.lucide) lucide.createIcons();
  }

  generateStatusCardHtml(acc, deep) {
    const borderClass = acc.disabled || acc.breaker_fails > 0 ? 'border-rose-500/30'
      : (acc.cooling ? 'border-amber-500/30' : 'border-emerald-500/30');

    // ---- 头部：昵称 + realm 域标签 + 状态徽章 ----
    const realmBadge = this.realmBadge(acc.realm);

    // ---- 配额区：池内 credits 与深度查询 quota 双来源 ----
    let quotaHtml = '';
    if (deep.quota) {
      const q = deep.quota;
      const pct = q.total > 0 ? Math.min(100, Math.round((q.remain / q.total) * 100)) : 0;
      const barColor = pct > 50 ? 'bg-emerald-500' : (pct > 20 ? 'bg-amber-500' : 'bg-rose-500');
      quotaHtml = `
        <div class="space-y-2">
          <div class="flex items-center justify-between text-[11px] gap-2">
            <span class="text-slate-400 flex-shrink-0">配额余量</span>
            <span class="font-mono font-semibold text-slate-700 dark:text-slate-200 truncate">
              ${q.remain} <span class="text-slate-400 font-normal">/ ${q.total}</span>
            </span>
          </div>
          <div class="h-1.5 rounded-full bg-slate-100 dark:bg-slate-800 overflow-hidden">
            <div class="h-full ${barColor} transition-all" style="width:${pct}%"></div>
          </div>
          <div class="flex items-center justify-between text-[10px] text-slate-400 gap-2">
            <span class="truncate" title="${this.escapeHtml(q.plan || '')}">${this.escapeHtml(q.plan || '未知套餐')}</span>
            <span class="flex-shrink-0">已用 ${q.used || 0}</span>
          </div>
          ${q.cycle_end_time ? `<div class="text-[10px] text-slate-400">周期至 ${this.escapeHtml(q.cycle_end_time)}</div>` : ''}
        </div>`;
    } else {
      quotaHtml = this.deepQueryHint('配额', deep.errors && deep.errors.quota);
    }

    // 池内积分（后端 /status 的 credits，与上面的实时 quota 互为参照）
    const creditsRow = (typeof acc.credits === 'number')
      ? `<div class="flex items-center justify-between text-[10px] text-slate-400 pt-1">
           <span>池内积分</span>
           <span class="font-mono text-slate-500 dark:text-slate-400" title="网关内存中记录的积分，供选号权重使用">${acc.credits}</span>
         </div>`
      : '';

    // ---- 签到区 ----
    let checkinHtml = '';
    if (deep.checkin) {
      const c = deep.checkin;
      const done = c.checked_in;
      checkinHtml = `
        <div class="flex items-center justify-between gap-3">
          <div class="space-y-0.5 min-w-0">
            <div class="text-[11px] text-slate-400">每日签到</div>
            <div class="text-xs font-medium ${done ? 'text-emerald-500' : 'text-amber-500'}">
              ${done ? '今日已签到' : '今日未签到'} · 连登 ${c.streak_days || 0} 天
            </div>
            ${c.total_credits ? `<div class="text-[10px] text-slate-400">活动累计 +${c.total_credits}</div>` : ''}
          </div>
          ${done ? '' : `
            <button onclick="app.doCheckin('${this.escapeHtml(acc.uid)}')" class="px-2.5 py-1 rounded-lg bg-amber-500 hover:bg-amber-400 text-white text-[10px] font-semibold transition-colors flex-shrink-0">
              去签到 +${c.daily_credit || 0}
            </button>`}
        </div>`;
    } else {
      checkinHtml = this.deepQueryHint('签到', deep.errors && deep.errors.checkin);
    }

    // ---- 模型级限流（上游 6004 独立冷却）----
    const rlHtml = this.rateLimitedModelsHtml(acc);

    // ---- 模型区 ----
    let modelsHtml = '';
    if (deep.models && deep.models.length) {
      const shown = deep.models.slice(0, 6);
      modelsHtml = `
        <div class="space-y-1.5">
          <div class="flex items-center justify-between text-[11px]">
            <span class="text-slate-400">可用模型</span>
            <span class="font-mono text-slate-500">${deep.models.length} 个</span>
          </div>
          <div class="flex flex-wrap gap-1">
            ${shown.map(m => `<span class="px-1.5 py-0.5 rounded text-[10px] font-mono bg-slate-100 dark:bg-slate-800 text-slate-500 dark:text-slate-400">${this.escapeHtml(m)}</span>`).join('')}
            ${deep.models.length > shown.length ? `<span class="px-1.5 py-0.5 text-[10px] text-slate-400">+${deep.models.length - shown.length}</span>` : ''}
          </div>
        </div>`;
    } else {
      modelsHtml = this.deepQueryHint('模型', deep.errors && deep.errors.models);
    }

    // ---- 异常信息：停用原因 / 最后一次错误 ----
    let alertHtml = '';
    if (acc.disabled && acc.disabled_reason) {
      alertHtml = `<div class="p-2 rounded-lg bg-rose-500/10 border border-rose-500/20 text-[10px] text-rose-600 dark:text-rose-400 break-words">
        <span class="font-semibold">停用原因：</span>${this.escapeHtml(acc.disabled_reason)}</div>`;
    }
    const lastErr = this.fmtTime(acc.last_err);
    if (lastErr) {
      alertHtml += `<div class="p-2 rounded-lg bg-amber-500/10 border border-amber-500/20 text-[10px] text-amber-600 dark:text-amber-400 break-words">
        <span class="font-semibold">最近错误：</span>${this.escapeHtml(lastErr)}</div>`;
    }

    return `
      <div class="p-4 sm:p-5 rounded-2xl bg-white dark:bg-dark-card border ${borderClass} shadow-sm space-y-4">
        <!-- 头部 -->
        <div class="flex items-start justify-between gap-2">
          <div class="flex items-center gap-2.5 min-w-0">
            <div class="w-9 h-9 rounded-xl bg-indigo-50 dark:bg-indigo-950/60 text-indigo-600 dark:text-indigo-400 font-bold flex items-center justify-center text-sm flex-shrink-0">
              ${this.escapeHtml((acc.nickname || 'CB').slice(0, 2).toUpperCase())}
            </div>
            <div class="min-w-0">
              <h4 class="text-xs font-bold text-slate-800 dark:text-slate-100 truncate">${this.escapeHtml(acc.nickname || 'CodeBuddy 账号')}</h4>
              <div class="flex items-center gap-1.5 mt-0.5">
                ${realmBadge}
                <span class="text-[10px] font-mono text-slate-400 truncate" title="${this.escapeHtml(acc.uid)}">${this.escapeHtml((acc.uid || '--').slice(0, 18))}</span>
              </div>
            </div>
          </div>
          <div class="flex-shrink-0">${this.accountStateBadge(acc)}</div>
        </div>

        ${alertHtml}

        <!-- 配额 -->
        <div class="pb-3 border-b border-slate-100 dark:border-slate-800/80">
          ${quotaHtml}${creditsRow}
        </div>

        <!-- 签到 -->
        <div class="pb-3 border-b border-slate-100 dark:border-slate-800/80">${checkinHtml}</div>

        <!-- 模型级限流 -->
        ${rlHtml}

        <!-- 模型 -->
        ${modelsHtml}

        <!-- 运行态 -->
        <div class="pt-3 border-t border-slate-100 dark:border-slate-800/80 grid grid-cols-3 gap-2 text-center">
          <div>
            <div class="text-[10px] text-slate-400">成功率</div>
            <div class="text-xs font-mono font-semibold text-slate-700 dark:text-slate-200">${this.successRate(acc)}</div>
          </div>
          <div>
            <div class="text-[10px] text-slate-400">在途</div>
            <div class="text-xs font-mono font-semibold text-slate-700 dark:text-slate-200">${acc.in_flight || 0}</div>
          </div>
          <div>
            <div class="text-[10px] text-slate-400">软冷却</div>
            <div class="text-xs font-mono font-semibold text-slate-700 dark:text-slate-200">${acc.soft_streak || 0}</div>
          </div>
        </div>

        ${acc.last_success && !String(acc.last_success).startsWith('0001') ? `
        <div class="text-[10px] text-slate-400 text-center">最近成功 ${this.escapeHtml(this.fmtTime(acc.last_success) || '')}</div>` : ''}
      </div>
    `;
  }

  // realm 域标签：cn / global（后端 realm 字段，空则视为 cn）
  realmBadge(realm) {
    const r = (realm || 'cn').toLowerCase();
    if (r === 'global') {
      return `<span class="px-1.5 py-0.5 rounded text-[9px] font-semibold bg-violet-500/10 text-violet-500 flex-shrink-0" title="global 域（workbuddy.ai）">GLOBAL</span>`;
    }
    return `<span class="px-1.5 py-0.5 rounded text-[9px] font-semibold bg-sky-500/10 text-sky-500 flex-shrink-0" title="cn 域（CodeBuddy 国内）">CN</span>`;
  }

  // 模型级限流展示（上游 6004 独立冷却：每账号每模型独立计时）
  rateLimitedModelsHtml(acc) {
    const list = acc.rate_limited_models;
    if (!Array.isArray(list) || list.length === 0) return '';
    const items = list.map(m => {
      const name = typeof m === 'string' ? m : (m.model || '');
      const reset = typeof m === 'object' ? this.fmtResetAt(m) : '';
      return `<span class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] font-mono bg-amber-500/10 text-amber-600 dark:text-amber-400"
                    title="${this.escapeHtml(name)}${reset ? ' · ' + this.escapeHtml(reset) : ''}">
        ${this.escapeHtml(name)}${reset ? ` <span class="opacity-70">${this.escapeHtml(reset)}</span>` : ''}
      </span>`;
    }).join('');
    return `
      <div class="p-2.5 rounded-xl bg-amber-500/5 border border-amber-500/20 space-y-1.5">
        <div class="flex items-center justify-between text-[11px] gap-2">
          <span class="text-amber-600 dark:text-amber-400 font-medium flex items-center gap-1">
            <i data-lucide="gauge-circle" class="w-3 h-3"></i>模型限流中
          </span>
          <span class="font-mono text-amber-500">${list.length} 个</span>
        </div>
        <div class="flex flex-wrap gap-1">${items}</div>
      </div>`;
  }

  // 把限流模型的 reset_at 格式化为「还剩 X 分钟」
  fmtResetAt(m) {
    const raw = m.reset_at || m.resetAt || m.until;
    if (!raw || String(raw).startsWith('0001')) return '';
    const t = new Date(raw).getTime();
    if (isNaN(t)) return '';
    const left = Math.round((t - Date.now()) / 1000);
    if (left <= 0) return '已恢复';
    return this.formatDuration(left);
  }

  // 安全格式化时间戳：无效/零值返回空串（后端零值时间是 0001-01-01）
  fmtTime(v) {
    if (!v || String(v).startsWith('0001')) return '';
    const t = new Date(v);
    if (isNaN(t.getTime())) return '';
    return t.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
  }

  // 深度查询失败/未查询时的占位提示
  deepQueryHint(label, errMsg) {
    if (errMsg) {
      return `<div class="text-[11px] text-amber-600 dark:text-amber-400">${this.escapeHtml(label)}查询失败: ${this.escapeHtml(errMsg)}</div>`;
    }
    return `<div class="text-[11px] text-slate-400">${this.escapeHtml(label)}：点击上方「刷新全部」获取</div>`;
  }

  successRate(acc) {
    const ok = acc.success_count || 0;
    const err = acc.err_total || 0;
    const total = ok + err;
    if (total === 0) return '--';
    return ((ok / total) * 100).toFixed(1) + '%';
  }

  // 手动签到
  async doCheckin(uid) {
    try {
      const res = await this.apiRequest(`/api/accounts/checkin?uid=${encodeURIComponent(uid)}`, { method: 'POST' });
      const data = await res.json();
      if (res.ok && data.success) {
        this.showToast(data.message || '签到成功', 'success');
        // 签到后清掉该账号缓存并重查，让用户立刻看到新状态
        await this.loadAccountStatus(uid);
      } else {
        this.showToast(data.message || '签到失败', 'error');
      }
    } catch (err) {
      this.showToast('签到请求失败: ' + err.message, 'error');
    }
  }

  // ===== 凭证管理 =====

  async fetchCredentials() {
    const list = document.getElementById('credentials-list');
    try {
      const res = await this.apiRequest('/api/credentials');
      const data = await res.json();
      if (!res.ok || !data.success) throw new Error(data.message || `HTTP ${res.status}`);

      const creds = data.credentials || [];
      const navCount = document.getElementById('nav-credentials-count');
      if (navCount) navCount.textContent = data.total || 0;

      if (list) {
        if (creds.length === 0) {
          list.innerHTML = `<div class="p-8 text-center text-slate-400 text-xs">
            暂无凭证。点击右上角「登录添加账号」或使用下方手动粘贴方式。
          </div>`;
        } else {
          list.innerHTML = creds.map(c => this.generateCredentialRowHtml(c)).join('');
        }
      }

      // 异常文件提示
      const brokenBox = document.getElementById('credentials-broken');
      const brokenList = document.getElementById('credentials-broken-list');
      if (brokenBox && brokenList) {
        const broken = data.broken || [];
        if (broken.length) {
          brokenBox.classList.remove('hidden');
          brokenList.innerHTML = broken.map(b =>
            `<div>${this.escapeHtml(b.file_name)} — ${this.escapeHtml(b.reason)}</div>`).join('');
        } else {
          brokenBox.classList.add('hidden');
        }
      }

      if (window.lucide) lucide.createIcons();
    } catch (err) {
      if (list) {
        list.innerHTML = `<div class="p-8 text-center text-rose-500 text-xs">加载失败: ${this.escapeHtml(err.message)}</div>`;
      }
    }
  }

  generateCredentialRowHtml(c) {
    const stateMap = {
      valid:    ['有效', 'bg-emerald-500/10 text-emerald-500'],
      expiring: ['即将过期', 'bg-amber-500/10 text-amber-500'],
      expired:  ['已过期', 'bg-rose-500/10 text-rose-500'],
      unknown:  ['未知', 'bg-slate-500/10 text-slate-400'],
    };
    const [stateText, stateClass] = stateMap[c.token_state] || stateMap.unknown;

    const tokenInfo = c.token_state === 'expired'
      ? '需要重新登录'
      : (c.token_left_sec ? `剩余 ${this.formatDuration(c.token_left_sec)}` : '--');

    return `
      <div class="p-4 flex items-center justify-between gap-4 hover:bg-slate-50 dark:hover:bg-slate-900/40 transition-colors">
        <div class="flex items-center gap-3 min-w-0">
          <div class="w-9 h-9 rounded-xl bg-indigo-50 dark:bg-indigo-950/60 text-indigo-600 dark:text-indigo-400 font-bold flex items-center justify-center text-sm flex-shrink-0">
            ${this.escapeHtml((c.nickname || 'CB').slice(0, 2).toUpperCase())}
          </div>
          <div class="min-w-0">
            <div class="text-xs font-bold text-slate-800 dark:text-slate-100 flex items-center gap-2">
              <span class="truncate">${this.escapeHtml(c.nickname || '未命名账号')}</span>
              <span class="px-1.5 py-0.5 rounded text-[10px] font-semibold ${stateClass} flex-shrink-0">${stateText}</span>
            </div>
            <div class="text-[10px] font-mono text-slate-400 truncate" title="${this.escapeHtml(c.file_name)}">
              ${this.escapeHtml(c.file_name)}
            </div>
            <div class="text-[10px] text-slate-400 mt-0.5 flex items-center gap-2 flex-wrap">
              <span>Token ${this.escapeHtml(tokenInfo)}</span>
              ${c.has_refresh ? '<span class="text-emerald-500">可自动续期</span>' : '<span class="text-amber-500">无 refreshToken</span>'}
              ${c.enterprise_id ? `<span>企业 ${this.escapeHtml(c.enterprise_id)}</span>` : '<span>个人</span>'}
            </div>
          </div>
        </div>
        <button onclick="app.deleteCredential('${this.escapeHtml(c.file_name)}', '${this.escapeHtml(c.nickname || c.uid)}')"
                class="flex-shrink-0 p-2 rounded-lg text-slate-400 hover:text-rose-500 hover:bg-rose-500/10 transition-colors" title="删除此凭证">
          <i data-lucide="trash-2" class="w-4 h-4"></i>
        </button>
      </div>
    `;
  }

  async deleteCredential(fileName, label) {
    if (!confirm(`确认删除凭证「${label}」？\n\n该账号将立即从池中移除，正在进行的请求不受影响。`)) return;
    try {
      const res = await this.apiRequest('/api/credentials/delete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ file_name: fileName }),
      });
      const data = await res.json();
      if (res.ok && data.success) {
        this.showToast('凭证已删除', 'success');
        await this.fetchCredentials();
        this.fetchStatus();
      } else {
        this.showToast(data.message || '删除失败', 'error');
      }
    } catch (err) {
      this.showToast('删除请求失败: ' + err.message, 'error');
    }
  }

  async reloadCredentials() {
    try {
      const res = await this.apiRequest('/api/credentials/reload', { method: 'POST' });
      const data = await res.json();
      if (res.ok && data.success) {
        this.showToast(data.message || '已重新加载', 'success');
        await this.fetchCredentials();
        this.fetchStatus();
      } else {
        this.showToast(data.message || '重载失败', 'error');
      }
    } catch (err) {
      this.showToast('重载请求失败: ' + err.message, 'error');
    }
  }

  async uploadCredential() {
    const token = (document.getElementById('cred-token')?.value || '').trim();
    if (!token) {
      this.showToast('请填写 accessToken', 'warning');
      return;
    }
    try {
      const res = await this.apiRequest('/api/credentials/upload', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          access_token: token,
          refresh_token: (document.getElementById('cred-refresh')?.value || '').trim(),
          nickname: (document.getElementById('cred-nickname')?.value || '').trim(),
          uid: (document.getElementById('cred-uid')?.value || '').trim(),
        }),
      });
      const data = await res.json();
      if (res.ok && data.success) {
        this.showToast(data.message || '凭证已保存', 'success');
        ['cred-token', 'cred-refresh', 'cred-nickname', 'cred-uid'].forEach(id => {
          const el = document.getElementById(id);
          if (el) el.value = '';
        });
        await this.fetchCredentials();
        this.fetchStatus();
      } else {
        this.showToast(data.message || '保存失败', 'error');
      }
    } catch (err) {
      this.showToast('保存请求失败: ' + err.message, 'error');
    }
  }

  // ===== OAuth 登录 =====

  openOAuthModal() {
    this.oauthState = null;
    const modal = document.getElementById('oauth-modal');
    if (modal) modal.classList.remove('hidden');
    document.getElementById('oauth-step-start')?.classList.remove('hidden');
    document.getElementById('oauth-step-wait')?.classList.add('hidden');
    if (window.lucide) lucide.createIcons();
  }

  closeOAuthModal() {
    const modal = document.getElementById('oauth-modal');
    if (modal) modal.classList.add('hidden');
  }

  async startOAuth() {
    const btn = document.getElementById('oauth-start-btn');
    const txt = document.getElementById('oauth-start-text');
    if (btn) btn.disabled = true;
    if (txt) txt.textContent = '生成中…';
    try {
      const res = await this.apiRequest('/api/credentials/oauth/start', { method: 'POST' });
      const data = await res.json();
      if (!res.ok || !data.success) throw new Error(data.message || `HTTP ${res.status}`);

      this.oauthState = data.auth_state;
      const link = document.getElementById('oauth-link');
      if (link) { link.href = data.auth_url; link.textContent = data.auth_url; }

      document.getElementById('oauth-step-start')?.classList.add('hidden');
      document.getElementById('oauth-step-wait')?.classList.remove('hidden');
      if (window.lucide) lucide.createIcons();

      // 自动打开登录页（部分浏览器会拦截，故同时保留手动按钮）
      window.open(data.auth_url, '_blank', 'noopener');
      this.showToast('登录链接已生成，请在新窗口完成登录', 'info');
    } catch (err) {
      this.showToast('生成登录链接失败: ' + err.message, 'error');
    } finally {
      if (btn) btn.disabled = false;
      if (txt) txt.textContent = '重新生成登录链接';
    }
  }

  openOAuthLink() {
    const link = document.getElementById('oauth-link');
    if (link && link.href && link.href !== '#') {
      window.open(link.href, '_blank', 'noopener');
    }
  }

  async pollOAuth() {
    const btn = document.getElementById('oauth-poll-btn');
    const txt = document.getElementById('oauth-poll-text');
    const hint = document.getElementById('oauth-poll-hint');
    if (!this.oauthState) {
      this.showToast('请先生成登录链接', 'warning');
      return;
    }
    if (btn) btn.disabled = true;
    if (txt) txt.textContent = '检查中…';
    try {
      const res = await this.apiRequest('/api/credentials/oauth/poll', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ auth_state: this.oauthState }),
      });
      const data = await res.json();

      if (res.ok && data.success && data.pending) {
        if (hint) hint.textContent = '尚未检测到登录，请先在浏览器完成登录后再点一次';
        this.showToast(data.message || '等待登录完成…', 'info');
      } else if (res.ok && data.success && !data.pending) {
        this.showToast(data.message || '登录成功！', 'success');
        this.closeOAuthModal();
        await this.fetchCredentials();
        this.fetchStatus();
      } else {
        this.showToast(data.message || '登录失败', 'error');
      }
    } catch (err) {
      this.showToast('检查失败: ' + err.message, 'error');
    } finally {
      if (btn) btn.disabled = false;
      if (txt) txt.textContent = '我已登录，检查状态';
    }
  }

  generateAccountRowHtml(acc) {
    // 后端 /status 的实际字段（此前前端引用的 in_cooldown/cooldown_remaining 等并不存在）
    const badge = this.accountStateBadge(acc);

    return `
      <div class="flex items-center justify-between p-3 rounded-xl bg-slate-50 dark:bg-slate-900/50 border border-slate-100 dark:border-slate-800/80">
        <div class="flex items-center gap-3">
          <div class="w-8 h-8 rounded-lg bg-gradient-to-br from-indigo-500/20 to-purple-500/20 text-indigo-500 dark:text-indigo-400 font-bold flex items-center justify-center text-xs">
            ${this.escapeHtml((acc.nickname || acc.uid || 'U').charAt(0).toUpperCase())}
          </div>
          <div>
            <div class="text-xs font-bold text-slate-800 dark:text-slate-200 flex items-center gap-1.5">
              <span>${this.escapeHtml(acc.nickname || '腾讯开发者')}</span>
            </div>
            <div class="text-[10px] text-slate-500 dark:text-slate-400 font-mono mt-0.5">
              ${this.escapeHtml((acc.uid || '').slice(0, 18))}${(acc.uid || '').length > 18 ? '…' : ''}
            </div>
          </div>
        </div>
        <div>${badge}</div>
      </div>
    `;
  }

  // 账号状态徽章：统一走这里，避免各处重复判断（此前分散在三处，口径不一致）
  accountStateBadge(acc) {
    if (acc.disabled) {
      return `<span class="px-2 py-0.5 rounded-full text-[10px] font-semibold bg-rose-500/10 text-rose-500">已停用</span>`;
    }
    if (acc.breaker_fails > 0) {
      return `<span class="px-2 py-0.5 rounded-full text-[10px] font-semibold bg-rose-500/10 text-rose-500">熔断中 ×${acc.breaker_fails}</span>`;
    }
    if (acc.cooling) {
      const left = acc.cool_remaining_sec ? this.formatDuration(acc.cool_remaining_sec) : '冷却';
      return `<span class="px-2 py-0.5 rounded-full text-[10px] font-semibold bg-amber-500/10 text-amber-500">冷却 ${left}</span>`;
    }
    return `<span class="px-2 py-0.5 rounded-full text-[10px] font-semibold bg-emerald-500/10 text-emerald-500">就绪</span>`;
  }

  // 秒数 → 人类可读时长
  formatDuration(sec) {
    if (!sec || sec <= 0) return '--';
    if (sec < 60) return `${sec} 秒`;
    if (sec < 3600) return `${Math.floor(sec / 60)} 分钟`;
    if (sec < 86400) return `${(sec / 3600).toFixed(1)} 小时`;
    return `${(sec / 86400).toFixed(1)} 天`;
  }

  escapeHtml(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
  }

  generateAccountCardHtml(acc) {
    const borderClass = acc.disabled ? 'border-rose-500/30'
      : (acc.breaker_fails > 0 ? 'border-rose-500/30'
      : (acc.cooling ? 'border-amber-500/30' : 'border-emerald-500/30'));

    const rows = [];
    rows.push(['成功 / 错误', `${acc.success_count || 0} / ${acc.err_total || 0}`]);
    rows.push(['在途请求', acc.in_flight || 0]);
    rows.push(['连续软冷却', `${acc.soft_streak || 0} 次`]);
    if (acc.cool_kind) rows.push(['冷却类型', acc.cool_kind]);
    if (acc.cool_remaining_sec) rows.push(['剩余冷却', this.formatDuration(acc.cool_remaining_sec)]);
    if (acc.breaker_fails) rows.push(['熔断失败数', acc.breaker_fails]);
    if (acc.reason) rows.push(['状态原因', acc.reason]);

    const lastSuccess = acc.last_success && !acc.last_success.startsWith('0001')
      ? new Date(acc.last_success).toLocaleString('zh-CN') : '--';

    return `
      <div class="p-5 rounded-2xl bg-white dark:bg-dark-card border ${borderClass} shadow-sm flex flex-col justify-between space-y-4">
        <div>
          <div class="flex items-center justify-between mb-3">
            <div class="flex items-center gap-2.5">
              <div class="w-9 h-9 rounded-xl bg-indigo-50 dark:bg-indigo-950/60 text-indigo-600 dark:text-indigo-400 font-bold flex items-center justify-center text-sm">
                ${this.escapeHtml((acc.nickname || 'CB').slice(0, 2).toUpperCase())}
              </div>
              <div>
                <h4 class="text-xs font-bold text-slate-800 dark:text-slate-100">${this.escapeHtml(acc.nickname || 'CodeBuddy 账号')}</h4>
                <div class="text-[10px] font-mono text-slate-400" title="${this.escapeHtml(acc.uid)}">UID: ${this.escapeHtml((acc.uid || '--').slice(0, 20))}</div>
              </div>
            </div>
            ${this.accountStateBadge(acc)}
          </div>

          <div class="space-y-2 text-xs py-3 border-y border-slate-100 dark:border-slate-800/80">
            ${rows.map(([k, v]) => `
              <div class="flex justify-between gap-3">
                <span class="text-slate-400 flex-shrink-0">${this.escapeHtml(k)}:</span>
                <span class="font-mono text-slate-700 dark:text-slate-300 text-right truncate" title="${this.escapeHtml(v)}">${this.escapeHtml(v)}</span>
              </div>`).join('')}
          </div>
        </div>

        <div class="pt-1 flex items-center justify-between text-[11px] text-slate-400">
          <span>最近成功: ${this.escapeHtml(lastSuccess)}</span>
          <button onclick="app.loadAccountStatus('${this.escapeHtml(acc.uid)}')" class="text-indigo-500 hover:text-indigo-400 font-medium transition-colors">查详情</button>
        </div>
      </div>
    `;
  }

  // 3. 拉取模型列表
  async fetchModels() {
    try {
      const res = await this.apiRequest('/v1/models');
      if (!res.ok) return;
      const data = await res.json();
      this.cachedModels = data.data || [];
      // 恢复上次选择的排序按钮高亮（setModelSort 会重绘）
      this.setModelSort(this.modelSort || 'default');

      // 同步到 playground 下拉菜单
      const select = document.getElementById('play-model-select');
      if (select && this.cachedModels.length > 0) {
        select.innerHTML = this.cachedModels.map(m => `
          <option value="${m.id}">${m.id}</option>
        `).join('');
      }
    } catch (err) {
      console.warn('拉取 /v1/models 失败:', err);
    }
  }

  // 解析模型 id 的 realm 前缀（网关路由协议：cn:xxx / global:xxx）。
  // 无前缀视为 cn（与后端 resolveModel 同语义）。
  parseModelId(id) {
    const s = String(id || '');
    const i = s.indexOf(':');
    if (i > 0) {
      const p = s.slice(0, i);
      if (p === 'cn' || p === 'global') return { realm: p, bare: s.slice(i + 1) };
    }
    return { realm: 'cn', bare: s };
  }

  // 切换模型排序方式
  setModelSort(mode) {
    this.modelSort = mode;
    localStorage.setItem('wb_model_sort', mode);
    // 更新按钮高亮
    document.querySelectorAll('.model-sort-btn').forEach(b => {
      const on = b.getAttribute('data-sort') === mode;
      b.className = b.className
        .replace(/\s*bg-white\b|\s*dark:bg-dark-card\b|\s*shadow-sm\b|\s*text-indigo-600\b|\s*dark:text-indigo-400\b|\s*font-semibold\b/g, '')
        .trim();
      if (on) b.className += ' bg-white dark:bg-dark-card shadow-sm text-indigo-600 dark:text-indigo-400 font-semibold';
    });
    this.renderModels(this.cachedModels);
  }

  // sortModels 按当前排序方式处理模型列表。
  //   default : 保持后端顺序（与官方控制台一致）
  //   credits : 按官方积分倍率升序（便宜的在前面；无倍率的排最后）
  sortModels(list) {
    const mode = this.modelSort || 'default';
    if (mode !== 'credits') return list.slice();
    return list.slice().sort((a, b) => {
      const av = (typeof a.credits === 'number') ? a.credits : Infinity;
      const bv = (typeof b.credits === 'number') ? b.credits : Infinity;
      if (av !== bv) return av - bv;
      return String(a.id).localeCompare(String(b.id));
    });
  }

  // creditsBadge 渲染积分倍率徽章。
  // 无固定倍率（如 auto：官方描述"积分倍率随之浮动"）显示"浮动"。
  creditsBadge(m) {
    if (typeof m.credits !== 'number') {
      return `<span class="px-2 py-0.5 rounded-lg text-[10px] font-semibold bg-slate-500/10 text-slate-400 border border-slate-500/20" title="官方未标注固定倍率，随任务动态浮动">倍率浮动</span>`;
    }
    const v = m.credits;
    const text = 'x' + v.toFixed(2);
    if (v === 0) {
      return `<span class="px-2 py-0.5 rounded-lg text-[10px] font-bold bg-emerald-500/15 text-emerald-500 border border-emerald-500/30" title="免费，不消耗积分">免费 ${text}</span>`;
    }
    // 便宜（<0.1）偏绿、中等（<1）偏蓝、贵（>=1）偏橙
    const cls = v < 0.1 ? 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/20'
      : v < 1 ? 'bg-sky-500/10 text-sky-600 dark:text-sky-400 border-sky-500/20'
      : 'bg-orange-500/10 text-orange-600 dark:text-orange-400 border-orange-500/20';
    return `<span class="px-2 py-0.5 rounded-lg text-[10px] font-semibold border ${cls}" title="官方积分倍率">${text}</span>`;
  }

  renderModels(models) {
    const container = document.getElementById('models-container');
    if (!container) return;

    const sorted = this.sortModels(models);

    // 统计按 realm 分组（用于导航角标与分组标题）
    const groups = { cn: [], global: [] };
    sorted.forEach(m => {
      const { realm } = this.parseModelId(m.id);
      (groups[realm] || groups.cn).push(m);
    });

    // 角标显示总数（只统计当前真正可用的）
    const totalEl = document.getElementById('stat-models-count');
    const navEl = document.getElementById('nav-models-count');
    if (totalEl) totalEl.textContent = models.length;
    if (navEl) navEl.textContent = models.length;

    if (models.length === 0) {
      container.innerHTML = `
        <div class="col-span-full text-center py-12 text-slate-400 text-xs space-y-1">
          <p>暂无可用模型</p>
          <p class="text-[11px]">请先在「凭证管理」中添加账号</p>
        </div>`;
      if (window.lucide) lucide.createIcons();
      return;
    }

    // 分组渲染：只渲染有模型的分组，避免出现空标题
    const sections = [];
    if (groups.cn.length) {
      sections.push(this.renderModelGroup('CN', 'cn', groups.cn,
        '国内域（codebuddy.cn）。<code class="font-mono">cn:</code> 前缀可省略，裸名即为 CN'));
    }
    if (groups.global.length) {
      sections.push(this.renderModelGroup('GLOBAL', 'global', groups.global,
        '国际域（workbuddy.ai）。<strong>必须带 <code class="font-mono">global:</code> 前缀</strong>，去掉会路由到 CN 池'));
    }

    container.className = 'space-y-6';
    container.innerHTML = sections.join('');
    if (window.lucide) lucide.createIcons();
  }

  // renderModelGroup 渲染一个 realm 分组（标题 + 说明 + 卡片网格）。
  renderModelGroup(label, realm, list, hint) {
    const isGlobal = realm === 'global';
    const badgeCls = isGlobal
      ? 'bg-violet-500/10 text-violet-500 border-violet-500/20'
      : 'bg-sky-500/10 text-sky-500 border-sky-500/20';

    const cards = list.map(m => this.renderModelCard(m, realm)).join('');

    return `
      <div class="space-y-3">
        <div class="flex flex-wrap items-center gap-2">
          <span class="px-2 py-0.5 rounded-lg text-[10px] font-semibold border ${badgeCls}">${label}</span>
          <span class="text-xs text-slate-400">${list.length} 个模型</span>
          <span class="text-[11px] text-slate-400 hidden sm:inline">${hint}</span>
        </div>
        <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">${cards}</div>
      </div>`;
  }

  // renderModelCard 单个模型卡片。model id 用大号等宽字显示，并配一个显眼的复制按钮
  // （复制的是**填进客户端就能用**的完整 id）。
  renderModelCard(m, realm) {
    const id = String(m.id || '');
    const { bare } = this.parseModelId(id);
    const isGlobal = realm === 'global';
    const idCls = isGlobal ? 'text-violet-600 dark:text-violet-400' : 'text-sky-600 dark:text-sky-400';

    const ctx = m.context_length ? this.formatNumberCompact(m.context_length) : '--';
    const maxOut = m.max_output_tokens ? this.formatNumberCompact(m.max_output_tokens) : '--';

    return `
      <div class="p-4 sm:p-5 rounded-2xl bg-white dark:bg-dark-card border border-slate-200 dark:border-dark-border shadow-sm flex flex-col justify-between hover:border-indigo-500/50 transition-colors">
        <div class="space-y-3">
          <!-- 顶部：倍率徽章 -->
          <div class="flex items-center justify-between gap-2">
            ${this.creditsBadge(m)}
            <span class="text-[10px] text-slate-400 font-mono">${this.escapeHtml(m.name || '')}</span>
          </div>

          <!-- model id 主体：等宽大字 + 复制按钮 -->
          <div class="flex items-start justify-between gap-2">
            <div class="min-w-0 flex-1">
              <div class="text-[10px] text-slate-400 mb-1">模型 ID</div>
              <code class="block text-sm font-bold font-mono ${idCls} break-all leading-snug">${this.escapeHtml(id)}</code>
            </div>
            <!-- id 走 data- 属性传递（HTML 上下文已转义），避免内联 JS 字符串的引号注入问题 -->
            <button data-model-id="${this.escapeHtml(id)}" onclick="app.copyModelId(this)"
                    class="flex-shrink-0 flex items-center gap-1 px-2.5 py-1.5 rounded-lg bg-indigo-600 hover:bg-indigo-500 text-white text-[10px] font-semibold transition-colors"
                    title="复制模型 ID：${this.escapeHtml(id)}">
              <i data-lucide="copy" class="w-3 h-3"></i>
              <span>复制</span>
            </button>
          </div>

          <!-- 前缀说明：让用户不必猜 cn: 前缀的含义与等价写法 -->
          <p class="text-[10px] text-slate-400 leading-relaxed">
            ${isGlobal
              ? `前缀 <code class="font-mono text-violet-500">global:</code> <strong>必须保留</strong>——去掉会路由到 CN 池。出站时前缀会被剥离。`
              : `<code class="font-mono text-sky-500">cn:</code> 前缀可省略，等价写法：<code class="font-mono">${this.escapeHtml(bare)}</code>（出站时会自动剥离前缀）`}
          </p>

          <!-- 规格 -->
          <div class="grid grid-cols-2 gap-2 text-[10px] text-slate-400">
            <div class="flex items-center justify-between">
              <span>上下文</span>
              <span class="font-mono text-slate-500 dark:text-slate-300">${ctx}</span>
            </div>
            <div class="flex items-center justify-between">
              <span>最大输出</span>
              <span class="font-mono text-slate-500 dark:text-slate-300">${maxOut}</span>
            </div>
          </div>

          ${isGlobal && bare !== id ? `
          <div class="p-2 rounded-lg bg-violet-500/5 border border-violet-500/15 text-[10px] text-violet-600 dark:text-violet-400 break-all">
            出站裸名：<code class="font-mono">${this.escapeHtml(bare)}</code>
          </div>` : ''}
        </div>

        <div class="mt-3 pt-3 border-t border-slate-100 dark:border-slate-800/80 flex items-center justify-between text-[11px] text-slate-400">
          <span>${this.escapeHtml(m.owned_by || 'workbuddy')}</span>
          <button data-model-id="${this.escapeHtml(id)}" onclick="app.useModelId(this)" class="text-indigo-500 hover:text-indigo-400 font-medium">在沙盒测试 →</button>
        </div>
      </div>`;
  }

  // copyModelId 从按钮的 data-model-id 取完整 id 并复制（客户端可直接填的形态）。
  copyModelId(btn) {
    const id = btn && btn.getAttribute('data-model-id');
    if (!id) return;
    this.copyText(id);
  }

  // useModelId 从按钮的 data-model-id 取 id 并送入沙盒。
  useModelId(btn) {
    const id = btn && btn.getAttribute('data-model-id');
    if (!id) return;
    this.useModelInPlayground(id);
  }

  filterModels() {
    const query = (document.getElementById('model-search-input')?.value || '').trim().toLowerCase();
    if (!query) { this.renderModels(this.cachedModels); return; }
    // 搜索时同时匹配完整 id 与裸名，方便用户按「gpt-5」这类片段找
    const filtered = this.cachedModels.filter(m => {
      const id = String(m.id || '').toLowerCase();
      const { bare } = this.parseModelId(m.id);
      return id.includes(query) || bare.toLowerCase().includes(query);
    });
    this.renderModels(filtered);
  }

  useModelInPlayground(modelId) {
    const select = document.getElementById('play-model-select');
    if (select) select.value = modelId;
    this.switchTab('playground');
  }

  // 4. API 测试沙盒 (Playground) 流式对话
  async sendChatMessage(event) {
    if (event) event.preventDefault();
    if (this.isChatStreaming) return;

    const input = document.getElementById('chat-input');
    const text = (input.value || '').trim();
    if (!text) return;

    const modelSelect = document.getElementById('play-model-select');
    const model = modelSelect ? modelSelect.value : 'deepseek-v3';

    // 渲染用户消息
    this.appendMessage('user', text);
    input.value = '';

    // 禁用发送按钮
    this.isChatStreaming = true;
    const sendBtn = document.getElementById('btn-send-chat');
    sendBtn.disabled = true;

    // 创建 Assistant 消息占位
    const assistantMsg = this.appendMessage('assistant', '');
    const contentEl = assistantMsg.querySelector('.msg-content');
    const cursor = document.createElement('span');
    cursor.className = 'cursor-blink';
    contentEl.appendChild(cursor);

    const metricsBar = document.getElementById('chat-metrics-bar');
    metricsBar.classList.remove('hidden');

    const startTime = performance.now();
    let firstTokenTime = null;
    let accumulatedContent = '';
    let tokenCount = 0;

    try {
      const payload = {
        model,
        messages: [
          { role: 'user', content: text }
        ],
        stream: true
      };

      const res = await this.apiRequest('/v1/chat/completions', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json'
        },
        body: JSON.stringify(payload)
      });

      if (!res.ok) {
        const errText = await res.text();
        throw new Error(`网关报错 (${res.status}): ${errText}`);
      }

      // 处理 SSE 流式响应
      const reader = res.body.getReader();
      const decoder = new TextDecoder('utf-8');
      let buffer = '';

      while (true) {
        const { value, done } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split('\n');
        buffer = lines.pop(); // 保留未完整的片段

        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed || trimmed.startsWith(':')) continue;

          if (trimmed === 'data: [DONE]') {
            break;
          }

          if (trimmed.startsWith('data: ')) {
            try {
              const parsed = JSON.parse(trimmed.slice(6));
              const delta = parsed.choices?.[0]?.delta?.content || '';
              if (delta) {
                if (!firstTokenTime) {
                  firstTokenTime = performance.now();
                  const ttft = Math.round(firstTokenTime - startTime);
                  document.getElementById('chat-metric-ttft').textContent = `首字耗时: ${ttft} ms`;
                }

                accumulatedContent += delta;
                tokenCount++;

                // 实时渲染 Markdown
                if (window.marked) {
                  contentEl.innerHTML = marked.parse(accumulatedContent);
                } else {
                  contentEl.textContent = accumulatedContent;
                }
                contentEl.appendChild(cursor);

                // 自动滚到底部
                const chatContainer = document.getElementById('chat-messages');
                chatContainer.scrollTop = chatContainer.scrollHeight;
              }
            } catch (jsonErr) {
              // 忽略偶尔非 JSON 行
            }
          }
        }
      }

      // 流式结束
      const totalTime = ((performance.now() - startTime) / 1000).toFixed(2);
      const speed = tokenCount > 0 ? (tokenCount / ((performance.now() - (firstTokenTime || startTime)) / 1000)).toFixed(1) : '--';

      document.getElementById('chat-metric-speed').textContent = `速率: ~${speed} tok/s`;
      document.getElementById('chat-metric-total').textContent = `总耗时: ${totalTime} s`;

    } catch (err) {
      contentEl.innerHTML = `<span class="text-rose-500 font-semibold">请求失败: ${err.message}</span>`;
    } finally {
      if (cursor.parentNode) cursor.remove();
      this.isChatStreaming = false;
      sendBtn.disabled = false;
      input.focus();
    }
  }

  appendMessage(role, content) {
    const container = document.getElementById('chat-messages');
    const wrapper = document.createElement('div');
    wrapper.className = `flex items-start gap-3 ${role === 'user' ? 'justify-end' : ''}`;

    const isUser = role === 'user';
    const avatar = isUser
      ? `<div class="w-8 h-8 rounded-lg bg-slate-200 dark:bg-slate-700 text-slate-600 dark:text-slate-300 flex items-center justify-center text-xs font-bold flex-shrink-0">我</div>`
      : `<div class="w-8 h-8 rounded-lg bg-indigo-600/20 text-indigo-500 flex items-center justify-center flex-shrink-0"><i data-lucide="bot" class="w-4 h-4"></i></div>`;

    wrapper.innerHTML = `
      ${!isUser ? avatar : ''}
      <div class="max-w-2xl ${isUser ? 'bg-indigo-600 text-white rounded-2xl rounded-tr-none px-4 py-2.5 text-xs' : 'bg-slate-100 dark:bg-slate-900 border border-slate-200/80 dark:border-slate-800 rounded-2xl rounded-tl-none px-4 py-3 text-xs leading-relaxed space-y-2 text-slate-800 dark:text-slate-200 prose-chat'}">
        <div class="msg-content">${content}</div>
      </div>
      ${isUser ? avatar : ''}
    `;

    container.appendChild(wrapper);
    container.scrollTop = container.scrollHeight;
    if (window.lucide) lucide.createIcons();
    return wrapper;
  }

  clearChat() {
    const container = document.getElementById('chat-messages');
    container.innerHTML = `
      <div class="flex items-start gap-3">
        <div class="w-8 h-8 rounded-lg bg-indigo-600/20 text-indigo-500 flex items-center justify-center flex-shrink-0">
          <i data-lucide="bot" class="w-4 h-4"></i>
        </div>
        <div class="max-w-2xl bg-slate-100 dark:bg-slate-900 border border-slate-200/80 dark:border-slate-800 rounded-2xl px-4 py-3 text-xs leading-relaxed space-y-2">
          <p class="font-semibold text-slate-800 dark:text-slate-200">对话已清空。</p>
          <p class="text-slate-500 dark:text-slate-400">你可以在下方随时输入新问题进行连通性测试。</p>
        </div>
      </div>
    `;
    document.getElementById('chat-metrics-bar').classList.add('hidden');
    if (window.lucide) lucide.createIcons();
  }

  // 5. 设置保存
  saveSettings() {
    const url = document.getElementById('setting-gateway-url').value.trim();
    const key = document.getElementById('setting-api-key').value.trim();

    if (!url) {
      this.showToast('网关地址不能为空', 'warning');
      return;
    }

    this.gatewayUrl = url.replace(/\/+$/, '');
    this.apiKey = key;

    localStorage.setItem('wb_gateway_url', this.gatewayUrl);
    localStorage.setItem('wb_api_key', this.apiKey);

    this.updateEndpointLabels();
    this.showToast('设置已保存，正在重新建立连接...', 'success');
    this.refreshAll();
  }

  resetSettings() {
    this.gatewayUrl = this.defaultGateway;
    this.apiKey = '';
    localStorage.removeItem('wb_gateway_url');
    localStorage.removeItem('wb_api_key');

    document.getElementById('setting-gateway-url').value = this.gatewayUrl;
    document.getElementById('setting-api-key').value = '';
    this.updateEndpointLabels();
    this.showToast('已恢复默认网关地址', 'info');
    this.refreshAll();
  }

  // 主题切换
  toggleTheme() {
    const isDark = document.documentElement.classList.toggle('dark');
    localStorage.setItem('wb_theme', isDark ? 'dark' : 'light');
    this.showToast(`已切换至${isDark ? '暗黑极客' : '明亮浅色'}模式`, 'info');
  }

  // 复制工具
  // 复制文本到剪贴板。
  //
  // 为什么必须多路径降级：navigator.clipboard 只在**安全上下文**（HTTPS 或
  // localhost）存在。本控制台最常见的访问方式是 http://192.168.x.x（局域网 IP），
  // 属于非安全上下文，此时 navigator.clipboard 为 undefined。
  // 原实现直接调 navigator.clipboard.writeText(...)，在非安全上下文会抛
  // TypeError，表现为「点了复制按钮没任何反应」。
  //
  // 三条路径依序尝试：
  //   1. Clipboard API（安全上下文，最干净）
  //   2. textarea + execCommand('copy')（非安全上下文下的传统方案）
  //   3. 弹出手动复制框（前两者都不可用时的兜底，保证功能不丢）
  copyText(text) {
    const value = String(text == null ? '' : text);
    if (!value) return;
    const done = () => this.showToast(`已复制: ${value}`, 'success');

    // 路径 1：现代 Clipboard API
    if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(value)
        .then(done)
        .catch(() => {
          if (this.copyViaTextarea(value)) done();
          else this.showManualCopy(value);
        });
      return;
    }

    // 路径 2：execCommand（非安全上下文）
    if (this.copyViaTextarea(value)) {
      done();
      return;
    }

    // 路径 3：手动复制兜底（不静默失败）
    this.showManualCopy(value);
  }

  // copyViaTextarea 用临时 textarea + document.execCommand('copy')。
  // 返回是否成功。该方法在非安全上下文下依然可用（Chrome/Edge/Safari 均支持）。
  copyViaTextarea(value) {
    let ta = null;
    try {
      ta = document.createElement('textarea');
      ta.value = value;
      ta.setAttribute('readonly', '');
      // 不能用 display:none / visibility:hidden——那样无法 select()；
      // 用固定定位移出视口，既不遮挡也不影响滚动。
      ta.style.position = 'fixed';
      ta.style.top = '0';
      ta.style.left = '0';
      ta.style.width = '1px';
      ta.style.height = '1px';
      ta.style.padding = '0';
      ta.style.border = 'none';
      ta.style.outline = 'none';
      ta.style.boxShadow = 'none';
      ta.style.background = 'transparent';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      ta.setSelectionRange(0, value.length);
      const ok = document.execCommand('copy');
      return !!ok;
    } catch (_) {
      return false;
    } finally {
      if (ta && ta.parentNode) ta.parentNode.removeChild(ta);
    }
  }

  // showManualCopy 弹出可手动复制的文本框（自动复制全不可用时的最后手段）。
  // 不静默失败：用户至少能看到并复制到内容。
  showManualCopy(value) {
    const old = document.getElementById('manual-copy-modal');
    if (old) old.remove();

    const wrap = document.createElement('div');
    wrap.id = 'manual-copy-modal';
    wrap.className = 'fixed inset-0 z-[60] bg-slate-900/60 backdrop-blur-sm flex items-center justify-center p-4';
    wrap.innerHTML = `
      <div class="w-full max-w-md rounded-2xl bg-white dark:bg-dark-card border border-slate-200 dark:border-dark-border shadow-2xl p-5 space-y-3">
        <div class="flex items-start justify-between gap-3">
          <div>
            <h3 class="text-sm font-bold">请手动复制</h3>
            <p class="text-[11px] text-slate-400 mt-1">当前浏览器环境不允许自动写入剪贴板（通常因为以 HTTP + IP 方式访问）。请按 Ctrl/Cmd+C 复制下方内容。</p>
          </div>
          <button id="manual-copy-close" class="p-1 rounded-lg text-slate-400 hover:text-slate-600 dark:hover:text-slate-200">
            <i data-lucide="x" class="w-4 h-4"></i>
          </button>
        </div>
        <textarea id="manual-copy-text" readonly
          class="w-full h-24 p-3 text-xs font-mono rounded-xl bg-slate-50 dark:bg-slate-900 border border-slate-200 dark:border-slate-800 focus:outline-none focus:border-indigo-500 resize-none"></textarea>
      </div>`;
    document.body.appendChild(wrap);

    const ta = document.getElementById('manual-copy-text');
    ta.value = value;
    ta.focus();
    ta.select();

    const close = () => wrap.remove();
    document.getElementById('manual-copy-close').onclick = close;
    wrap.onclick = (e) => { if (e.target === wrap) close(); };

    if (window.lucide) lucide.createIcons();
  }

  copyId(id) {
    const el = document.getElementById(id);
    if (el) {
      this.copyText(el.value || el.textContent);
    }
  }

  copyEndpoint() {
    this.copyText(`${this.gatewayUrl}/v1`);
  }

  copyCherryConfig() {
    const text = `API 域名: ${this.gatewayUrl}\nAPI Key: ${this.apiKey || '123'}\n模型: deepseek-v3, deepseek-r1`;
    this.copyText(text);
  }

  copyChatboxConfig() {
    const text = `API 主机: ${this.gatewayUrl}\nAPI Key: ${this.apiKey || '123'}`;
    this.copyText(text);
  }

  toggleKeyVisibility(inputId) {
    const el = document.getElementById(inputId);
    if (el) {
      el.type = el.type === 'password' ? 'text' : 'password';
    }
  }

  // 切换网关预设地址（如内网局域网 / 公网 DDNS 域名）
  setGatewayPreset(url) {
    document.getElementById('setting-gateway-url').value = url;
    this.showToast(`已填入预设地址: ${url}，点击「保存并重连」生效`, 'info');
  }

  // 随机生成安全强秘钥
  generateRandomKey() {
    const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
    let randomStr = '';
    for (let i = 0; i < 24; i++) {
      randomStr += chars.charAt(Math.floor(Math.random() * chars.length));
    }
    const newKey = `sk-${randomStr}`;
    document.getElementById('setting-api-key').value = newKey;
    this.showToast('已生成随机强秘钥，请点击「同步并保存至路由器网关」', 'info');
  }

  // 从路由器拉取当前生效的 API Key
  async fetchRouterKey(silent = false) {
    try {
      const res = await this.apiRequest('/api/key');
      if (res.ok) {
        const data = await res.json();
        const key = data.api_key || '';
        const inputKey = document.getElementById('setting-api-key');
        if (inputKey) inputKey.value = key;
        this.apiKey = key;
        localStorage.setItem('wb_api_key', key);
        this.updateEndpointLabels();
        if (!silent) {
          this.showToast(key ? `成功读取路由器网关密钥: ${key}` : '路由器网关当前处于免密模式', 'success');
        }
      } else if (!silent) {
        throw new Error(`HTTP ${res.status}`);
      }
    } catch (err) {
      if (!silent) {
        this.showToast('无法从网关拉取密钥', 'warning');
      }
    }
  }

  // 一键将 API Key 推送同步到路由器网关并生效
  async syncKeyToRouter() {
    const key = (document.getElementById('setting-api-key')?.value || '').trim();
    const btn = document.getElementById('btn-sync-key');
    if (btn) {
      btn.disabled = true;
      btn.innerHTML = `<i data-lucide="loader-2" class="w-3.5 h-3.5 animate-spin"></i> 同步中...`;
    }

    try {
      const res = await this.apiRequest('/api/key', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ api_key: key })
      });

      const data = await res.json();
      if (res.ok && data.success) {
        this.apiKey = key;
        localStorage.setItem('wb_api_key', key);
        this.updateEndpointLabels();
        this.showToast('🎉 密钥已同步到路由器网关并立即生效！', 'success');
        setTimeout(() => this.refreshAll(), 500);
      } else {
        throw new Error(data.message || '同步失败');
      }
    } catch (err) {
      this.showToast(`同步失败: ${err.message}`, 'error');
    } finally {
      if (btn) {
        btn.disabled = false;
        btn.innerHTML = `<i data-lucide="send" class="w-3.5 h-3.5"></i> 同步并保存至路由器网关`;
        if (window.lucide) lucide.createIcons();
      }
    }
  }

  // ==========================================
  // 用量统计 (Usage Analytics) 模块
  // ==========================================
  setUsageRange(range) {
    this.usageRange = range;
    this.usageRecOffset = 0; // 换时间范围回到明细第一页
    document.querySelectorAll('.usage-range-btn').forEach(btn => {
      if (btn.getAttribute('data-range') === range) {
        btn.classList.add('active', 'bg-white', 'dark:bg-dark-card', 'shadow-sm', 'text-indigo-600', 'dark:text-indigo-400', 'font-semibold');
        btn.classList.remove('text-slate-500', 'dark:text-slate-400');
      } else {
        btn.classList.remove('active', 'bg-white', 'dark:bg-dark-card', 'shadow-sm', 'text-indigo-600', 'dark:text-indigo-400', 'font-semibold');
        btn.classList.add('text-slate-500', 'dark:text-slate-400');
      }
    });
    const labelMap = {
      '1h': '最近 1 小时',
      'today': '今日 00:00 至今',
      '24h': '最近 24 小时',
      '7d': '最近 7 天',
      'all': '全部历史记录'
    };
    const lbl = document.getElementById('usage-chart-range-label');
    if (lbl) lbl.textContent = labelMap[range] || range;
    this.fetchUsage(range);
  }

  async fetchUsage(range = this.usageRange || '24h') {
    try {
      const res = await this.apiRequest(`/api/usage?range=${range}`);
      if (res.ok) {
        const data = await res.json();
        this.cachedUsage = data;
        this.renderUsageView(data);
      }
    } catch (err) {
      console.warn('fetchUsage error:', err);
    }
  }

  renderUsageView(data) {
    if (!data) return;
    const summary = data.range_summary || {};

    // 核心数字卡片
    const totalReq = summary.total_requests || 0;
    const totalTok = summary.total_tokens || 0;
    const promptTok = summary.prompt_tokens || 0;
    const compTok = summary.completion_tokens || 0;
    const credit = Number(summary.total_credit || 0);

    const setText = (id, v) => { const el = document.getElementById(id); if (el) el.textContent = v; };

    setText('usage-total-requests', totalReq.toLocaleString());
    setText('usage-total-credit', credit ? credit.toFixed(2) : '0');
    setText('usage-completion-tokens', this.formatNumberCompact(compTok));

    const elCompRaw = document.getElementById('usage-completion-tokens-raw');
    if (elCompRaw) elCompRaw.textContent = compTok ? `(${compTok.toLocaleString()})` : '';

    // prompt_tokens: 语义上含会话上下文重复计数，作为次要指标并显式标注
    const elPrompt = document.getElementById('usage-prompt-tokens');
    if (elPrompt) elPrompt.textContent = this.formatNumberCompact(promptTok);
    const elPromptRaw = document.getElementById('usage-prompt-tokens-raw');
    if (elPromptRaw) elPromptRaw.textContent = promptTok ? `(${promptTok.toLocaleString()})` : '';
    setText('usage-prompt-note', summary.prompt_tokens_note || '');

    // 总 tokens（含重复计数）作为参考值
    const elTok = document.getElementById('usage-total-tokens');
    if (elTok) elTok.textContent = this.formatNumberCompact(totalTok);

    // 平均耗时
    const avg = summary.avg_duration_ms || 0;
    setText('usage-avg-duration', avg ? (avg / 1000).toFixed(2) + ' s' : '--');

    // 平均每次消耗积分（成本视角的"单次成本"）
    setText('usage-avg-credit', totalReq ? (credit / totalReq).toFixed(4) : '--');

    const elUpdated = document.getElementById('usage-last-updated');
    if (elUpdated) {
      const t = this.fmtTime(data.last_updated);
      const step = data.bucket_seconds ? this.formatBucketStep(data.bucket_seconds) : '';
      elUpdated.textContent = [t ? `数据更新于 ${t}` : '', step ? `分桶粒度：${step}` : '']
        .filter(Boolean).join(' · ');
    }

    // 趋势图表：以积分为主指标
    this.renderUsageChart(data.time_series || []);

    // 模型排行（按积分排序，更有成本意义）
    this.renderModelBreakdown(data.model_breakdown || [], totalTok);

    // 账号排行
    this.renderAccountBreakdown(data.account_breakdown || [], totalTok);

    // 请求明细（与聚合视图同步刷新）
    this.fetchUsageRecords();

    if (window.lucide) lucide.createIcons();
  }

  // ===== 请求明细（秒级）=====

  // debouncedFetchUsageRecords 输入框防抖，避免每敲一个字符就打一次后端
  debouncedFetchUsageRecords() {
    clearTimeout(this._usageRecTimer);
    this._usageRecTimer = setTimeout(() => {
      this.usageRecOffset = 0; // 筛选条件变化时回到第一页
      this.fetchUsageRecords();
    }, 350);
  }

  // usageRecPage 翻页（delta = -1 / +1）
  usageRecPage(delta) {
    const next = (this.usageRecOffset || 0) + delta * (this.usageRecLimit || 200);
    if (next < 0) return;
    if (this.usageRecTotal && next >= this.usageRecTotal) return;
    this.usageRecOffset = next;
    this.fetchUsageRecords();
  }

  async fetchUsageRecords() {
    const tbody = document.getElementById('usage-rec-tbody');
    if (!tbody) return;

    const limit = this.usageRecLimit || 200;
    const offset = this.usageRecOffset || 0;
    const model = (document.getElementById('usage-rec-model')?.value || '').trim();
    const range = this.usageRange || '24h';

    const qs = new URLSearchParams({ range, limit: String(limit), offset: String(offset) });
    if (model) qs.set('model', model);

    try {
      const res = await this.apiRequest('/api/usage/records?' + qs.toString());
      const data = await res.json();
      if (!res.ok || !data.success) throw new Error(data.message || `HTTP ${res.status}`);

      const recs = data.records || [];
      this.usageRecTotal = data.total || 0;

      // 摘要
      const sumEl = document.getElementById('usage-rec-summary');
      if (sumEl) {
        sumEl.textContent = `匹配 ${this.usageRecTotal} 条${model ? `（模型含「${model}」）` : ''}，当前显示第 ${offset + 1}–${offset + recs.length} 条`;
      }

      if (recs.length === 0) {
        tbody.innerHTML = `<tr><td colspan="6" class="py-6 text-center text-slate-400 font-sans">该时间范围内没有记录</td></tr>`;
      } else {
        tbody.innerHTML = recs.map(r => `
          <tr class="hover:bg-slate-50/50 dark:hover:bg-slate-800/40 transition-colors">
            <td class="py-2 text-slate-500 dark:text-slate-400 whitespace-nowrap">${this.fmtFullTime(r.timestamp)}</td>
            <td class="py-2 text-slate-700 dark:text-slate-200" title="${this.escapeHtml(r.model)}">${this.escapeHtml((r.model || '-').slice(0, 22))}</td>
            <td class="py-2 text-right text-slate-700 dark:text-slate-300">${(r.completion_tokens || 0).toLocaleString()}</td>
            <td class="py-2 text-right text-slate-400" title="客户端每轮重发全部历史，该值含重复计数">${(r.prompt_tokens || 0).toLocaleString()}</td>
            <td class="py-2 text-right text-amber-600 dark:text-amber-400">${(r.credit || 0).toFixed(3)}</td>
            <td class="py-2 text-right text-slate-500 dark:text-slate-400">${r.duration_ms ? (r.duration_ms / 1000).toFixed(1) + 's' : '--'}</td>
          </tr>`).join('');
      }

      // 分页状态
      const info = document.getElementById('usage-rec-page-info');
      if (info) {
        const from = this.usageRecTotal === 0 ? 0 : offset + 1;
        const to = offset + recs.length;
        info.textContent = `${from}–${to} / 共 ${this.usageRecTotal} 条`;
      }
      const prev = document.getElementById('usage-rec-prev');
      const next = document.getElementById('usage-rec-next');
      if (prev) prev.disabled = offset <= 0;
      if (next) next.disabled = offset + limit >= this.usageRecTotal;
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="6" class="py-6 text-center text-rose-500 font-sans">加载失败：${this.escapeHtml(err.message)}</td></tr>`;
    }
  }

  // fmtFullTime 秒级完整时间（明细表用）
  fmtFullTime(ts) {
    if (!ts) return '--';
    const d = new Date(ts * 1000);
    if (isNaN(d.getTime())) return '--';
    const p = (n) => String(n).padStart(2, '0');
    return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  }

  // formatBucketStep 把秒数步长转成人类可读（"5 分钟" / "1 小时" / "1 天"）
  formatBucketStep(sec) {
    if (!sec) return '';
    if (sec < 60) return sec + ' 秒';
    if (sec < 3600) return Math.round(sec / 60) + ' 分钟';
    if (sec < 86400) return Math.round(sec / 3600) + ' 小时';
    return Math.round(sec / 86400) + ' 天';
  }

  formatNumberCompact(num) {
    if (!num) return '0';
    if (num >= 1000000) return (num / 1000000).toFixed(2) + 'M';
    if (num >= 1000) return (num / 1000).toFixed(1) + 'k';
    return num.toLocaleString();
  }

  renderUsageChart(buckets) {
    const container = document.getElementById('usage-chart-container');
    if (!container) return;

    if (!buckets || buckets.length === 0) {
      container.innerHTML = `<div class="w-full h-full flex items-center justify-center text-xs text-slate-400">暂无用量记录</div>`;
      return;
    }

    // 主指标用「消耗积分」而非 total_tokens：
    // prompt_tokens 含会话上下文重复计数（客户端每轮重发全部历史），
    // 用它画图会把趋势严重放大且随会话长度漂移；credit 是真实扣费，最能反映成本走势。
    const useCredit = buckets.some(b => (b.credit || 0) > 0);
    const metricOf = (b) => useCredit ? (b.credit || 0) : (b.completion_tokens || 0);
    const maxTokens = Math.max(1, ...buckets.map(metricOf));
    const count = buckets.length;

    // 构建 SVG 折线面积图
    const width = 800;
    const height = 180;
    const padTop = 20;
    const padBottom = 25;
    const padLeft = 10;
    const padRight = 10;
    const plotW = width - padLeft - padRight;
    const plotH = height - padTop - padBottom;

    const points = buckets.map((b, i) => {
      const x = padLeft + (plotW / (count - 1 || 1)) * i;
      const y = padTop + plotH - (metricOf(b) / maxTokens) * plotH;
      return { x, y, bucket: b };
    });

    let dArea = `M ${points[0].x} ${padTop + plotH} `;
    let dLine = `M ${points[0].x} ${points[0].y} `;

    for (let i = 0; i < points.length; i++) {
      dLine += `L ${points[i].x} ${points[i].y} `;
      dArea += `L ${points[i].x} ${points[i].y} `;
    }
    dArea += `L ${points[points.length - 1].x} ${padTop + plotH} Z`;

    let xLabelsHtml = '';
    const step = count > 12 ? Math.ceil(count / 6) : 1;
    for (let i = 0; i < count; i += step) {
      const p = points[i];
      xLabelsHtml += `<text x="${p.x}" y="${height - 4}" text-anchor="middle" font-size="10" fill="currentColor" class="text-slate-400 font-mono">${p.bucket.label}</text>`;
    }

    const circlesHtml = points.map(p => {
      const c = p.bucket.credit || 0;
      const tip = useCredit
        ? `${p.bucket.label}: ${c.toFixed(3)} 积分, ${p.bucket.call_count} 次调用`
        : `${p.bucket.label}: ${(p.bucket.completion_tokens || 0).toLocaleString()} 输出 Tokens, ${p.bucket.call_count} 次调用`;
      return `<circle cx="${p.x}" cy="${p.y}" r="3.5" class="chart-point fill-white dark:fill-dark-card stroke-indigo-500 hover:r-5 transition-all cursor-pointer" stroke-width="2" data-info="${tip}" />`;
    }).join('');

    container.innerHTML = `
      <svg viewBox="0 0 ${width} ${height}" class="w-full h-full overflow-visible">
        <defs>
          <linearGradient id="tokenAreaGrad" x1="0%" y1="0%" x2="0%" y2="100%">
            <stop offset="0%" stop-color="#6366f1" stop-opacity="0.35"/>
            <stop offset="100%" stop-color="#6366f1" stop-opacity="0.0"/>
          </linearGradient>
        </defs>
        <!-- 网格背景线 -->
        <line x1="${padLeft}" y1="${padTop}" x2="${width - padRight}" y2="${padTop}" stroke="currentColor" class="text-slate-100 dark:text-slate-800/80" stroke-dasharray="4 4" />
        <line x1="${padLeft}" y1="${padTop + plotH / 2}" x2="${width - padRight}" y2="${padTop + plotH / 2}" stroke="currentColor" class="text-slate-100 dark:text-slate-800/80" stroke-dasharray="4 4" />
        <line x1="${padLeft}" y1="${padTop + plotH}" x2="${width - padRight}" y2="${padTop + plotH}" stroke="currentColor" class="text-slate-200 dark:text-slate-800" />

        <!-- 面积与折线 -->
        <path d="${dArea}" fill="url(#tokenAreaGrad)" />
        <path d="${dLine}" fill="none" stroke="#6366f1" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" />

        <!-- 点与文本标签 -->
        ${circlesHtml}
        ${xLabelsHtml}
      </svg>
      <div id="usage-chart-tooltip" class="absolute hidden bg-slate-900 text-white text-[11px] px-2.5 py-1.5 rounded-lg pointer-events-none shadow-xl border border-slate-700 font-mono z-20"></div>
    `;

    // 绑定点悬浮提示
    const tooltip = document.getElementById('usage-chart-tooltip');
    container.querySelectorAll('.chart-point').forEach(pt => {
      pt.addEventListener('mouseenter', (e) => {
        const info = pt.getAttribute('data-info');
        if (tooltip && info) {
          tooltip.textContent = info;
          tooltip.classList.remove('hidden');
          const rect = pt.getBoundingClientRect();
          const pRect = container.getBoundingClientRect();
          tooltip.style.left = `${rect.left - pRect.left - tooltip.offsetWidth / 2 + 3}px`;
          tooltip.style.top = `${rect.top - pRect.top - 32}px`;
        }
      });
      pt.addEventListener('mouseleave', () => {
        if (tooltip) tooltip.classList.add('hidden');
      });
    });
  }

  renderModelBreakdown(models, totalTokens) {
    const tbody = document.getElementById('usage-model-tbody');
    const countEl = document.getElementById('usage-model-count');
    if (countEl) countEl.textContent = `${models.length} 个模型`;
    if (!tbody) return;

    if (models.length === 0) {
      tbody.innerHTML = `<tr><td colspan="4" class="py-4 text-center text-slate-400 font-sans">暂无模型调用数据</td></tr>`;
      return;
    }

    const colors = ['bg-indigo-500', 'bg-blue-500', 'bg-purple-500', 'bg-emerald-500', 'bg-amber-500', 'bg-rose-500'];

    tbody.innerHTML = models.map((m, idx) => {
      const pct = m.percentage ? m.percentage.toFixed(1) : '0.0';
      const color = colors[idx % colors.length];
      return `
        <tr class="hover:bg-slate-50/50 dark:hover:bg-slate-800/40 transition-colors">
          <td class="py-2.5 flex items-center gap-2">
            <span class="w-2 h-2 rounded-full ${color}"></span>
            <span class="font-bold text-slate-700 dark:text-slate-200">${m.model}</span>
          </td>
          <td class="py-2.5 text-right text-slate-500 dark:text-slate-400">${m.call_count}</td>
          <td class="py-2.5 text-right font-semibold text-amber-600 dark:text-amber-400">${(m.credit || 0).toFixed(3)}</td>
          <td class="py-2.5 text-right text-slate-500 dark:text-slate-400">${this.formatNumberCompact(m.completion_tokens || 0)}</td>
          <td class="py-2.5 text-right">
            <div class="flex items-center justify-end gap-2">
              <span class="text-[11px] text-slate-400">${pct}%</span>
              <div class="w-12 h-1.5 rounded-full bg-slate-100 dark:bg-slate-800 overflow-hidden">
                <div class="h-full ${color} rounded-full" style="width: ${pct}%"></div>
              </div>
            </div>
          </td>
        </tr>
      `;
    }).join('');
  }

  renderAccountBreakdown(accounts, totalTokens) {
    const tbody = document.getElementById('usage-account-tbody');
    const countEl = document.getElementById('usage-account-count');
    if (countEl) countEl.textContent = `${accounts.length} 个账号`;
    if (!tbody) return;

    if (accounts.length === 0) {
      tbody.innerHTML = `<tr><td colspan="4" class="py-4 text-center text-slate-400 font-sans">暂无账号调用数据</td></tr>`;
      return;
    }

    tbody.innerHTML = accounts.map(a => {
      const pct = a.percentage ? a.percentage.toFixed(1) : '0.0';
      const shortUID = a.uid.length > 8 ? a.uid.slice(0, 8) : a.uid;
      return `
        <tr class="hover:bg-slate-50/50 dark:hover:bg-slate-800/40 transition-colors">
          <td class="py-2.5 flex items-center gap-2">
            <span class="px-1.5 py-0.5 rounded bg-emerald-500/10 text-emerald-500 text-[10px] font-mono">UID</span>
            <span class="font-bold text-slate-700 dark:text-slate-200" title="${a.uid}">${shortUID}</span>
          </td>
          <td class="py-2.5 text-right text-slate-500 dark:text-slate-400">${a.call_count}</td>
          <td class="py-2.5 text-right font-semibold text-amber-600 dark:text-amber-400">${(a.credit || 0).toFixed(3)}</td>
          <td class="py-2.5 text-right text-slate-500 dark:text-slate-400">${this.formatNumberCompact(a.completion_tokens || 0)}</td>
          <td class="py-2.5 text-right">
            <div class="flex items-center justify-end gap-2">
              <span class="text-[11px] text-slate-400">${pct}%</span>
              <div class="w-12 h-1.5 rounded-full bg-slate-100 dark:bg-slate-800 overflow-hidden">
                <div class="h-full bg-emerald-500 rounded-full" style="width: ${pct}%"></div>
              </div>
            </div>
          </td>
        </tr>
      `;
    }).join('');
  }

  async clearUsageHistory() {
    if (!confirm('确定要清空历史 Token 用量统计记录吗？此操作不可撤销。')) {
      return;
    }
    try {
      const res = await this.apiRequest('/api/usage/clear', { method: 'POST' });
      if (res.ok) {
        this.showToast('用量历史已成功重置', 'success');
        this.fetchUsage();
      } else {
        throw new Error(`HTTP ${res.status}`);
      }
    } catch (err) {
      this.showToast('清空失败: ' + err.message, 'error');
    }
  }

  // Toast 气泡
  showToast(msg, type = 'info') {
    const toast = document.getElementById('toast');
    if (!toast) return;

    const bgMap = {
      success: 'bg-emerald-600 text-white',
      warning: 'bg-amber-500 text-white',
      error: 'bg-rose-600 text-white',
      info: 'bg-slate-800 text-slate-100 border border-slate-700'
    };

    toast.className = `fixed bottom-6 right-6 z-50 transform transition-all duration-300 px-4 py-2.5 rounded-xl text-xs font-medium flex items-center gap-2 shadow-xl ${bgMap[type] || bgMap.info}`;
    toast.textContent = msg;
    toast.style.opacity = '1';
    toast.style.transform = 'translateY(0)';

    clearTimeout(this.toastTimer);
    this.toastTimer = setTimeout(() => {
      toast.style.opacity = '0';
      toast.style.transform = 'translateY(1rem)';
    }, 2800);
  }
}

// 实例化应用
const app = new WorkBuddyApp();
