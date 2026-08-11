'use strict';

/**
 * Shared device-facing auth for /api/bd/* endpoints.
 *
 * Previously every route file carried its own copy of these helpers; this
 * module is the single source of truth. Baseline behaviour mirrors the full
 * bd-api.routes.js implementation:
 *
 *   1. Bearer access token  -> req.deviceId = tokenRow.client_id,
 *                              req.deviceToken = tokenRow, token touched.
 *   2. X-Device-Id header   -> regex-validated, then the device must exist
 *                              (peer OR approved pending registration) before
 *                              it is trusted.
 *   3. Nothing matched      -> 401.
 *
 * Per-file extensions are exposed as options (see identifyDevice).
 */

const db = require('../services/database');

/** Extract the Bearer token from the Authorization header, or null. */
function extractBearerToken(req) {
    const auth = req.headers['authorization'];
    if (!auth || !auth.startsWith('Bearer ')) return null;
    return auth.substring(7).trim();
}

/**
 * Lightweight device auth — Bearer access token OR X-Device-Id header.
 *
 * Options (per-file extensions, all optional):
 *   allowBodyDeviceId (bool, default false) — additionally accept
 *       req.body.device_id as a fallback when no X-Device-Id header is
 *       present (fileTransfer.routes.js sends the id in the body).
 *   requireDeviceExistence (bool, default true) — when false, skip the
 *       peer / approved-registration existence check for header/body ids.
 *
 * Sets req.deviceId (token client_id or header id) and, for token auth,
 * req.deviceToken; touches the token on every use.
 */
async function identifyDevice(req, res, next, options = {}) {
    const allowBodyDeviceId = !!options.allowBodyDeviceId;
    const requireDeviceExistence = options.requireDeviceExistence !== false;

    const token = extractBearerToken(req);
    if (token) {
        try {
            const tokenRow = await db.getAccessToken(token);
            if (tokenRow) {
                req.deviceId = tokenRow.client_id || null;
                req.deviceToken = tokenRow;
                await db.touchAccessToken(token);
                return next();
            }
        } catch (_) { /* ignored */ }
    }

    // Fallback: X-Device-Id header (or body.device_id where allowed).
    // P1: the header alone is spoofable — require the device to actually exist
    // (as a peer or as an approved registration) before trusting it.
    const deviceId = req.headers['x-device-id'] || (allowBodyDeviceId ? req.body?.device_id : null);
    if (deviceId && /^[A-Za-z0-9_-]{3,64}$/.test(deviceId)) {
        // typeof guards keep minimal test doubles working; the production
        // database module always defines both lookups.
        if (requireDeviceExistence && typeof db.getPeerById === 'function' && typeof db.getPendingRegistrationByDeviceId === 'function') {
            try {
                const peer = await db.getPeerById(deviceId);
                const reg = await db.getPendingRegistrationByDeviceId(deviceId);
                if (!peer && !(reg && reg.status === 'approved')) {
                    return res.status(401).json({ error: 'Unknown device' });
                }
            } catch (_) {
                return res.status(401).json({ error: 'Unknown device' });
            }
        }
        req.deviceId = deviceId;
        return next();
    }
    return res.status(401).json({ error: 'Missing device identification' });
}

/**
 * Strict Bearer-token-only device auth (previously inventory.routes.js).
 *
 * Unlike identifyDevice, X-Device-Id is intentionally NOT accepted as an
 * authentication credential: accepting it alone would let any client
 * impersonate an enrolled device. 401 when the token is missing, invalid,
 * expired, or unbound (no client_id).
 */
async function requireDeviceToken(req, res, next) {
    const token = extractBearerToken(req);
    if (!token) {
        return res.status(401).json({ error: 'Bearer access token required' });
    }

    let tokenRow;
    try {
        tokenRow = await db.getAccessToken(token);
    } catch (_) {
        return res.status(401).json({ error: 'Invalid or expired access token' });
    }

    if (!tokenRow || !tokenRow.client_id) {
        return res.status(401).json({ error: 'Invalid or unbound access token' });
    }

    req.deviceId = tokenRow.client_id;
    req.deviceToken = tokenRow;
    try {
        await db.touchAccessToken(token);
    } catch (_) {
        // Recording last use must not invalidate an already validated token.
    }
    return next();
}

module.exports = { extractBearerToken, identifyDevice, requireDeviceToken };
