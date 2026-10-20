#!/usr/bin/env node
// 看板脚本真求值烟测（可选工具，需要 node）：用最小 DOM stub 跑一遍脚本顶层，
// 捕获 TDZ / 未定义引用这类**求值顺序**问题——
// gofmt、node --check、单元测试都只查语法，查不出这个
// （2026-10-20：i18n 字典里写成 t("key_note_ok") 自调用 ⇒ 整个脚本抛
//  "Cannot access 't' before initialization" ⇒ 页面 JS 全死、看板打不开）。
//
// 用法: node scripts/dash_smoke.js [web/dashboard/index.html]
const fs = require('fs');
const path = process.argv[2] || 'web/dashboard/index.html';
const html = fs.readFileSync(path, 'utf8');
const m = html.match(/<script>([\s\S]*?)<\/script>/);
if (!m) { console.error('页面里没有 <script>'); process.exit(2); }
const mk = (tag='div') => { const el = { tagName:tag, style:{}, dataset:{},
  classList:{add(){},remove(){},toggle(){},contains(){return false}}, children:[],
  firstElementChild:null, lastElementChild:null,
  appendChild(c){this.children.push(c);return c}, insertBefore(c){this.children.push(c);return c},
  insertAdjacentHTML(){}, setAttribute(){}, getAttribute(){return null}, removeChild(){}, remove(){},
  querySelector(){return mk()}, querySelectorAll(){return []}, addEventListener(){}, removeEventListener(){},
  textContent:'', innerHTML:'', getContext(){return {}}, scrollIntoView(){} }; return el; };
global.document = { getElementById:()=>mk(), querySelector:()=>mk(), querySelectorAll:()=>[],
  createElement:()=>mk(), createElementNS:()=>mk(), body:mk(), documentElement:mk(),
  addEventListener(){}, title:'', location:{origin:'http://x',hash:'',search:'',pathname:'/'} };
global.window = global; global.self = global;
global.localStorage = { getItem:()=>null, setItem(){} };
global.location = { origin:'http://x', hash:'', search:'', pathname:'/', href:'http://x/' };
global.addEventListener = ()=>{};
global.IntersectionObserver = class { observe(){} };
global.ResizeObserver = class { observe(){} };
global.fetch = async () => ({ ok:true, status:200, json: async () => ({backends:[],metrics:{}}), headers:{get:()=>null} });
global.performance = { now:()=>Date.now() };
try {
  new Function(m[1])();
  console.log('✓ 看板脚本顶层求值通过（无 TDZ / 未定义引用）:', path);
  process.exit(0);
} catch (e) {
  console.error('✗ 顶层抛错:', e.constructor.name, e.message);
  process.exit(1);
}
