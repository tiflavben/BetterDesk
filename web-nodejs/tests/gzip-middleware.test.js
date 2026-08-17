/**
 * gzip-middleware.test.js — 零依赖 gzip 中间件专项单测（node 内置 http + zlib，无 express/jest）
 *
 * 被测对象：web-nodejs/server.js 的零依赖流式 gzip 中间件（R4 引入，2026-08-11
 * 隐式头部修复后版本，来源版本 git HEAD 8e3ced0，server.js:160-240）。
 *
 * 为什么复制中间件代码而不是 require：web-nodejs 无 node_modules（跑不了 express/jest），
 * 且 require('./server.js') 会启动整个面板服务；任务约定"直接复制中间件代码到测试脚本
 * 并注释来源版本"（见下方 ==== COPY_START / COPY_END ==== 区，逐字一致，仅把
 * `app.use((req, res, next) => {` 改为 `const gzipMiddleware = (req, res, next) => {`
 * 以便直接挂到 node http server 上）。文件末尾有 drift 检查：若 server.js 中间件被
 * 修改，会打印 [WARN] 提示人工同步（不阻塞测试）。
 *
 * 覆盖的响应风格（对应生产真实路径）：
 *   1. send 库式（静态文件）：setHeader(Content-Type+Content-Length) → 分段 write → end()
 *        —— express.static 的 send 库路径，触发 Node 隐式 writeHead（曾导致生产损坏的根因 1）
 *   2. 多段 write：setHeader(Content-Type) → 多次 write → end()   —— 流式/pipe 路径
 *   3. render 式：setHeader(Content-Type) → end(单 chunk)
 *        —— Express res.render/res.send 路径；Node 24 的 end(chunk) 内部走 write_() 辅助
 *           函数绕过覆写的 res.write（曾导致生产损坏的根因 2）
 *   4. 显式 writeHead 先行：writeHead(200, {Content-Type}) → end(chunk)
 *   5. 无 Content-Type：write(buf) → end()   —— 防损坏保证：永不出现"gzip 头 + 明文体"
 *
 * 每个风格都跑 gzip 请求（断言 Content-Encoding: gzip + 魔数 0x1f 0x8b + gunzipSync
 * 逐字节一致）与明文请求（断言无 Content-Encoding + 逐字节一致）；另覆盖
 * Content-Length 残留移除、非 GET、Range、二进制扩展名跳过。
 *
 * 运行：cd web-nodejs && node tests/gzip-middleware.test.js   （exit 0 = 全部通过）
 */

'use strict';

const http = require('http');
const fs = require('fs');
const path = require('path');

// =====================================================================
// ==== COPY_START —— 中间件代码逐字复制自 web-nodejs/server.js:160-240（来源版本 8e3ced0）====
// =====================================================================
const zlib = require('zlib');
const GZIP_TEXT_TYPES = /(?:text\/|application\/(?:javascript|json|xml|x-javascript|ecmascript)|image\/svg\+xml)/;
const GZIP_SKIP_EXT = /\.(?:png|jpe?g|gif|webp|woff2?|ttf|otf|eot|gz|br|zip|mp4|mp3|wasm)$/i;
const gzipMiddleware = (req, res, next) => {
    const acceptEncoding = (req.headers['accept-encoding'] || '').toLowerCase();
    if (!acceptEncoding.includes('gzip') || (req.method !== 'GET' && req.method !== 'HEAD') || req.headers.range) {
        return next();
    }
    const origWriteHead = res.writeHead;
    const origWrite = res.write.bind(res);
    const origEnd = res.end.bind(res);
    let gzipStream = null;
    let gzipping = false;
    // Node 隐式头部机制（关键设计约束）：当 handler 只 setHeader() 后直接
    // stream.pipe(res)（express.static 的 send 库正是如此，不显式调 writeHead），
    // 首次 res.write() 会先于 writeHead hook 触发，Node 随后才隐式调 writeHead。
    // 若 gzip 只在 writeHead 内启动，首段 body 已明文写进 socket、Content-Encoding:
    // gzip 头却随后才发出 → 浏览器按 gzip 解压明文失败（页面永久 loading）。
    // 因此 startGzip() 把"判定+初始化"抽成惰性辅助函数，write 与 writeHead 两条
    // 路径共用；幂等（gzipping 已置位直接返回 true），不会重复创建 gzipStream。
    const startGzip = (headers) => {
        if (gzipping) return true;
        let contentType = res.getHeader('Content-Type');
        if (headers && typeof headers === 'object' && !Buffer.isBuffer(headers) && !Array.isArray(headers)) {
            contentType = headers['Content-Type'] || headers['content-type'] || contentType;
        }
        if (!contentType || !GZIP_TEXT_TYPES.test(contentType) || res.getHeader('Content-Encoding') || GZIP_SKIP_EXT.test(req.path)) {
            return false;
        }
        gzipping = true;
        res.removeHeader('Content-Length');
        res.setHeader('Content-Encoding', 'gzip');
        res.setHeader('Vary', 'Accept-Encoding');
        if (headers && typeof headers === 'object' && !Buffer.isBuffer(headers) && !Array.isArray(headers)) {
            delete headers['Content-Length'];
            headers['Content-Encoding'] = 'gzip';
            headers['Vary'] = 'Accept-Encoding';
        }
        gzipStream = zlib.createGzip({ level: 6 });
        gzipStream.on('data', (chunk) => origWrite(chunk));
        gzipStream.on('end', () => origEnd());
        gzipStream.on('error', () => { try { origEnd(); } catch (_) { /* best effort */ } });
        return true;
    };
    res.writeHead = function (statusCode, headers) {
        startGzip(headers);
        return origWriteHead.call(this, statusCode, headers);
    };
    res.write = function (chunk, encoding, callback) {
        if (!gzipping && !gzipStream) {
            // 惰性启动：首次 write 到达时 send 库应已 setHeader('Content-Type')；
            // 若仍未设置（自定义 handler 先写数据未设类型），startGzip() 返回
            // false，本块数据走 origWrite 原样下发，绝不带 gzip 头发明文。
            startGzip();
        }
        if (gzipping && gzipStream) {
            if (typeof encoding === 'function') { callback = encoding; encoding = undefined; }
            gzipStream.write(chunk, encoding);
            if (typeof callback === 'function') process.nextTick(callback);
            return true;
        }
        return origWrite(chunk, encoding, callback);
    };
    res.end = function (chunk, encoding, callback) {
        if (!gzipping && !gzipStream) {
            // res.end(chunk) 在 Node 内部走 write_() 辅助函数而非 this.write ——
            // 覆写后的 res.write 会被绕过，因此 render 路径（Express res.send/
            // res.render 以 chunk 调 end）也必须在这里惰性尝试 startGzip()，
            // 否则 body 明文出站后 writeHead 才启动 gzip → 头体不匹配损坏。
            startGzip();
        }
        if (gzipping && gzipStream) {
            if (chunk) gzipStream.write(chunk, encoding || undefined);
            gzipStream.end();
            if (typeof callback === 'function') process.nextTick(callback);
            return this;
        }
        return origEnd(chunk, encoding, callback);
    };
    next();
};
// =====================================================================
// ==== COPY_END —— 复制区结束 ====
// =====================================================================

// =====================================================================
// Harness：真实 http.Server + 真实 ServerResponse（node 内置 http）
// =====================================================================

function startServer(handler) {
    return new Promise((resolve, reject) => {
        const server = http.createServer((req, res) => {
            // Express 语义：req.path 只含路径不含 query
            req.path = req.url.split('?')[0];
            gzipMiddleware(req, res, () => handler(req, res));
        });
        server.on('error', reject);
        server.listen(0, '127.0.0.1', () => {
            resolve({ server, port: server.address().port });
        });
    });
}

function closeServer(server) {
    return new Promise((resolve) => server.close(resolve));
}

function httpReq(port, pathname, opts) {
    return new Promise((resolve, reject) => {
        const req = http.request(
            Object.assign({ host: '127.0.0.1', port: port, path: pathname, method: 'GET' }, opts || {}),
            (res) => {
                const chunks = [];
                res.on('data', (c) => chunks.push(c));
                res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
            }
        );
        req.on('error', reject);
        req.end();
    });
}

// ---- 断言收集器 ----
const results = [];
function check(name, ok, detail) {
    results.push({ name: name, ok: ok });
    console.log((ok ? '[PASS] ' : '[FAIL] ') + name + (ok ? '' : '  —— ' + detail));
}

function assertGzipResponse(r, payload, name) {
    const okCE = r.headers['content-encoding'] === 'gzip';
    const okMagic = r.body.length >= 2 && r.body[0] === 0x1f && r.body[1] === 0x8b;
    let okDecode = false;
    try { okDecode = zlib.gunzipSync(r.body).equals(payload); } catch (_) { /* gunzip 失败 */ }
    check(name + '（gzip 请求）', okCE && okMagic && okDecode,
        'CE=' + (r.headers['content-encoding'] || '无') +
        ' magic=' + (r.body.length >= 2 ? r.body[0].toString(16) + ' ' + r.body[1].toString(16) : '响应过短') +
        ' 解压逐字节一致=' + okDecode);
}

function assertPlainResponse(r, payload, name, expectNotGzipMagic) {
    const noCE = !r.headers['content-encoding'];
    const okBody = r.body.equals(payload);
    let notMagic = true;
    if (expectNotGzipMagic && r.body.length >= 2 && r.body[0] === 0x1f && r.body[1] === 0x8b) {
        notMagic = false;
    }
    check(name + '（明文请求）', noCE && okBody && notMagic,
        'CE=' + (r.headers['content-encoding'] || '无') +
        ' 字节一致=' + okBody +
        (expectNotGzipMagic ? ' 非gzip魔数=' + notMagic : ''));
}

// ---- 测试载荷（可压缩文本，>1KB） ----
const JS_PAYLOAD = Buffer.from(
    Array.from({ length: 60 }, function (_, i) {
        return 'const line_' + i + " = 'gzip middleware test payload 0123456789 ABCDEFGHIJKLMNOPQRSTUVWXYZ';\n";
    }).join(''),
    'utf8'
);
const HTML_PAYLOAD = Buffer.from(
    '<!DOCTYPE html><html><head><title>gzip middleware test</title></head><body>' +
    Array.from({ length: 40 }, function (_, i) {
        return '<p>gzip middleware test paragraph ' + i + ' — 中文内容 also here for size.</p>';
    }).join('') +
    '</body></html>',
    'utf8'
);
const RAW_PAYLOAD = Buffer.from('no content type raw body — 未经压缩的原始字节 0123456789', 'utf8');

// ---- 五种响应风格 handler ----
const handlers = {
    // 1. send 库式（静态文件）：setHeader(CT+CL) → 分段 write → end()
    sendStyle: function (req, res) {
        res.setHeader('Content-Type', 'text/javascript; charset=utf-8');
        res.setHeader('Content-Length', JS_PAYLOAD.length);
        const half = Math.ceil(JS_PAYLOAD.length / 2);
        res.write(JS_PAYLOAD.slice(0, half));
        res.write(JS_PAYLOAD.slice(half));
        res.end();
    },
    // 2. 多段 write：setHeader(CT) → 3 段 write → end()
    multiWriteStyle: function (req, res) {
        res.setHeader('Content-Type', 'text/javascript; charset=utf-8');
        const third = Math.ceil(JS_PAYLOAD.length / 3);
        res.write(JS_PAYLOAD.slice(0, third));
        res.write(JS_PAYLOAD.slice(third, third * 2));
        res.write(JS_PAYLOAD.slice(third * 2));
        res.end();
    },
    // 3. render 式：setHeader(CT) → end(单 chunk)（Node 24 的 end(chunk) 绕过覆写的 res.write）
    renderStyle: function (req, res) {
        res.setHeader('Content-Type', 'text/html; charset=utf-8');
        res.end(HTML_PAYLOAD);
    },
    // 4. 显式 writeHead 先行：writeHead(200, {CT}) → end(chunk)
    writeHeadFirstStyle: function (req, res) {
        res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
        res.end(HTML_PAYLOAD);
    },
    // 5. 无 Content-Type（防损坏保证）
    noContentTypeStyle: function (req, res) {
        res.write(RAW_PAYLOAD);
        res.end();
    },
    // 6. 二进制扩展名（GZIP_SKIP_EXT + Content-Type 双过滤）
    binaryExtStyle: function (req, res) {
        res.setHeader('Content-Type', 'image/png');
        res.end(RAW_PAYLOAD);
    }
};

async function runCase(name, handler, payload, pathname) {
    const s = await startServer(handler);
    try {
        const gzipRes = await httpReq(s.port, pathname, { method: 'GET', headers: { 'Accept-Encoding': 'gzip' } });
        assertGzipResponse(gzipRes, payload, name);
        const plainRes = await httpReq(s.port, pathname, { method: 'GET', headers: {} });
        assertPlainResponse(plainRes, payload, name, false);
    } finally {
        await closeServer(s.server);
    }
}

// ---- drift 检查：server.js 的 gzip 中间件是否与测试内复制一致（WARN 不阻断） ----
function checkSourceDrift() {
    const serverPath = path.join(__dirname, '..', 'server.js');
    let src;
    try {
        src = fs.readFileSync(serverPath, 'utf8');
    } catch (e) {
        console.warn('[WARN] drift 检查无法读取 ' + serverPath + '：' + e.message);
        return;
    }
    const start = src.indexOf('const zlib = require(\'zlib\');');
    const endIdx = src.indexOf('\n// Static files', start);
    if (start < 0 || endIdx < 0) {
        console.warn('[WARN] drift 检查：server.js 中未找到 gzip 中间件块，跳过比对');
        return;
    }
    const mySource = fs.readFileSync(__filename, 'utf8');
    const cStart = mySource.indexOf('// ==== COPY_START');
    const cEnd = mySource.indexOf('// ==== COPY_END');
    if (cStart < 0 || cEnd < 0) {
        console.warn('[WARN] drift 检查：本文件 COPY 区标记缺失，跳过比对');
        return;
    }
    const zIdx = mySource.indexOf('const zlib = require(\'zlib\');', cStart);
    if (zIdx < 0) {
        console.warn('[WARN] drift 检查：本文件 COPY 区缺少中间件代码，跳过比对');
        return;
    }
    const copyBlock = mySource.slice(zIdx, mySource.indexOf('\n// ====', zIdx));
    const norm = function (s) { return s.replace(/\s+/g, ''); };
    let copyNorm = norm(copyBlock)
        .replace('constgzipMiddleware=(req,res,next)=>{', 'app.use((req,res,next)=>{')
        .replace(/\};$/, '});');
    const serverNorm = norm(src.slice(start, endIdx));
    if (serverNorm === copyNorm) {
        console.log('[INFO] drift 检查：server.js 中间件与测试内复制逐字一致（来源版本 8e3ced0）');
    } else {
        console.warn('[WARN] drift 检查：server.js 的 gzip 中间件已与测试内复制不一致！请把 server.js:160-240 的最新代码同步到本文件 COPY 区（本测试仍按旧复制执行，结果不代表当前 server.js）');
    }
}

// ---- 主流程 ----
(async function main() {
    checkSourceDrift();
    console.log('');

    await runCase('send 库式（静态文件）', handlers.sendStyle, JS_PAYLOAD, '/test.js');
    await runCase('多段 write（流式）', handlers.multiWriteStyle, JS_PAYLOAD, '/test.js');
    await runCase('render 式 end(chunk)', handlers.renderStyle, HTML_PAYLOAD, '/page');
    await runCase('显式 writeHead 先行', handlers.writeHeadFirstStyle, HTML_PAYLOAD, '/page');

    // 5. 无 Content-Type：gzip 请求与明文请求都必须原样透传（永不出现 gzip 头 + 明文体）
    {
        const s = await startServer(handlers.noContentTypeStyle);
        try {
            const gzipRes = await httpReq(s.port, '/raw', { method: 'GET', headers: { 'Accept-Encoding': 'gzip' } });
            assertPlainResponse(gzipRes, RAW_PAYLOAD, '无 Content-Type 防损坏', true);
            const plainRes = await httpReq(s.port, '/raw', { method: 'GET', headers: {} });
            check('无 Content-Type 明文透传', plainRes.body.equals(RAW_PAYLOAD) && !plainRes.headers['content-encoding'], '字节一致=' + plainRes.body.equals(RAW_PAYLOAD));
        } finally {
            await closeServer(s.server);
        }
    }

    // 6. 二进制扩展名跳过：image/png + /img.png，即使带 gzip 头也不压缩
    {
        const s = await startServer(handlers.binaryExtStyle);
        try {
            const res = await httpReq(s.port, '/img.png', { method: 'GET', headers: { 'Accept-Encoding': 'gzip' } });
            check('二进制扩展名跳过（/img.png + gzip 请求）', !res.headers['content-encoding'] && res.body.equals(RAW_PAYLOAD),
                'CE=' + (res.headers['content-encoding'] || '无') + ' 字节一致=' + res.body.equals(RAW_PAYLOAD));
        } finally {
            await closeServer(s.server);
        }
    }

    // 7. 非 GET 跳过：POST + Accept-Encoding: gzip 不压缩
    {
        const s = await startServer(handlers.renderStyle);
        try {
            const res = await httpReq(s.port, '/page', { method: 'POST', headers: { 'Accept-Encoding': 'gzip' } });
            check('非 GET 跳过（POST + gzip）', !res.headers['content-encoding'] && res.body.equals(HTML_PAYLOAD),
                'CE=' + (res.headers['content-encoding'] || '无') + ' 字节一致=' + res.body.equals(HTML_PAYLOAD));
        } finally {
            await closeServer(s.server);
        }
    }

    // 8. Range 跳过：Range 请求不压缩
    {
        const s = await startServer(handlers.renderStyle);
        try {
            const res = await httpReq(s.port, '/page', { method: 'GET', headers: { 'Accept-Encoding': 'gzip', 'Range': 'bytes=0-99' } });
            check('Range 跳过（Range + gzip）', !res.headers['content-encoding'] && res.body.equals(HTML_PAYLOAD),
                'CE=' + (res.headers['content-encoding'] || '无') + ' 字节一致=' + res.body.equals(HTML_PAYLOAD));
        } finally {
            await closeServer(s.server);
        }
    }

    // 9. send 库式 gzip 响应必须移除 handler 设置的 Content-Length（残留会截断/损坏）
    {
        const s = await startServer(handlers.sendStyle);
        try {
            const res = await httpReq(s.port, '/test.js', { method: 'GET', headers: { 'Accept-Encoding': 'gzip' } });
            const ok = res.headers['content-encoding'] === 'gzip' && res.headers['content-length'] === undefined && res.body.length > 0;
            check('gzip 响应移除 Content-Length（send 库式）', ok,
                'CL=' + (res.headers['content-length'] === undefined ? '已移除' : res.headers['content-length']) + ' 压缩后大小=' + res.body.length + 'B');
        } finally {
            await closeServer(s.server);
        }
    }

    const fails = results.filter(function (r) { return !r.ok; }).length;
    console.log('');
    console.log('==== 结果：' + (results.length - fails) + '/' + results.length + ' 通过 ====');
    process.exitCode = fails > 0 ? 1 : 0;
})().catch(function (err) {
    console.error('[FATAL] 测试执行异常：', err);
    process.exitCode = 1;
});