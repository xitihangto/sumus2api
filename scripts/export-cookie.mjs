#!/usr/bin/env node
/**
 * 从 ego lite 浏览器导出 Sumus cookie，写入 accounts/<name>.json。
 *
 * 用法：
 *   1. 在 ego lite 里登录 sumus.im（用 taskSpace 打开登录页完成登录）
 *   2. 记下 spaceId
 *   3. 运行：node scripts/export-cookie.mjs <spaceId> [账号名] [输出目录]
 *
 * 依赖 ego-browser CLI（ego lite 自带）。
 */
import { execFileSync } from 'node:child_process';
import { writeFileSync, mkdirSync, existsSync } from 'node:fs';

const spaceId = process.argv[2];
const name = process.argv[3] || `account-${Date.now()}`;
const outDir = process.argv[4] || 'accounts';

if (!spaceId) {
  console.error('用法: node scripts/export-cookie.mjs <spaceId> [账号名] [输出目录]');
  process.exit(1);
}

const script = `
const task = await taskSpace(${spaceId});
const page = task.page("p1");
const out = await page.cdp("Storage.getCookies", {});
const cookies = (out.cookies || []).filter(c => c.domain.includes("sumus"));
const auth = cookies.find(c => c.name === "__Secure-better-auth.session_token");
if (!auth) {
  console.error(JSON.stringify({ error: "no_session_cookie", found: cookies.map(c => c.name) }));
  process.exit(1);
}
console.log(JSON.stringify({
  session_token: auth.value,
  cookie_expires_at: Math.floor(auth.expires),
}));
`;

try {
  const raw = execFileSync('ego-browser', ['nodejs', '-e', script], {
    encoding: 'utf8',
    timeout: 60000,
  });
  const data = JSON.parse(raw.trim());
  if (data.error) {
    console.error('导出失败:', data.error, data.found || '');
    process.exit(1);
  }

  if (!existsSync(outDir)) mkdirSync(outDir, { recursive: true });
  const path = `${outDir}/${name}.json`;
  writeFileSync(path, JSON.stringify({
    session_token: data.session_token,
    cookie_expires_at: data.cookie_expires_at,
  }, null, 2), { mode: 0o600 });
  console.log(`已导出到 ${path}`);
  console.log(`cookie 到期时间: ${new Date(data.cookie_expires_at * 1000).toLocaleString()}`);
  console.log('注意: workspace_id / agent_id 会在网关启动时自动补齐');
} catch (err) {
  console.error('执行失败:', err.message);
  process.exit(1);
}
