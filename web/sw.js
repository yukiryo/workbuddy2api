/* WorkBuddy 控制台 Service Worker
 *
 * 设计原则（PWA 缓存安全）：
 *   1. 绝不缓存 /api/* 与 /v1/* —— 它们是动态且带鉴权的：
 *      缓存会导致登录态错乱、跨会话数据泄漏、以及把旧密钥当新值用。
 *   2. HTML 走 network-first：网关更新后刷新即得新版，不困在旧页面。
 *   3. 静态资源（图标 / 三个第三方库）走 cache-first：体积大且几乎不变。
 *   4. 离线时只回退到静态壳，不伪造任何接口数据。
 */

const VERSION = 'wb2api-v1';
const STATIC_CACHE = `${VERSION}-static`;

// 预缓存：仅体积小、必需的入口文件。三个大库按需缓存（首次访问即写入）。
const PRECACHE = [
  '/login.html',
  '/manifest.json',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/favicon-32.png',
];

// 明确排除的路径前缀：任何情况下都不进缓存
const NEVER_CACHE = ['/api/', '/v1/', '/healthz', '/status'];

// 可缓存的静态资源（第三方库 + 图标 + 样式脚本）
function isCacheableAsset(url) {
  return (
    url.pathname.startsWith('/assets/') ||
    url.pathname.startsWith('/icons/') ||
    url.pathname === '/style.css' ||
    url.pathname === '/app.js' ||
    url.pathname === '/manifest.json'
  );
}

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE).then((cache) =>
      // 逐个 add，单个失败不影响整体安装
      Promise.all(
        PRECACHE.map((p) =>
          cache.add(p).catch(() => {
            /* 忽略单个资源缺失 */
          })
        )
      )
    ).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys
            .filter((k) => k.startsWith('wb2api-') && k !== STATIC_CACHE)
            .map((k) => caches.delete(k))
        )
      )
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  const url = new URL(req.url);

  // 只处理同源 GET；其它（含所有 POST）直接放行不拦截
  if (req.method !== 'GET' || url.origin !== self.location.origin) return;

  // 鉴权与动态接口：绝不缓存，直连网络
  if (NEVER_CACHE.some((p) => url.pathname.startsWith(p))) return;

  // 静态资源：cache-first
  if (isCacheableAsset(url)) {
    event.respondWith(
      caches.match(req).then(
        (hit) =>
          hit ||
          fetch(req).then((res) => {
            if (res && res.ok && res.type === 'basic') {
              const copy = res.clone();
              caches.open(STATIC_CACHE).then((c) => c.put(req, copy));
            }
            return res;
          })
      )
    );
    return;
  }

  // 页面导航：network-first，失败时回退到缓存的壳
  if (req.mode === 'navigate') {
    event.respondWith(
      fetch(req)
        .then((res) => {
          // 只缓存成功且同源的页面
          if (res && res.ok && res.type === 'basic') {
            const copy = res.clone();
            caches.open(STATIC_CACHE).then((c) => c.put(req, copy));
          }
          return res;
        })
        .catch(() =>
          caches.match(req).then((hit) => hit || caches.match('/login.html'))
        )
    );
  }
});
