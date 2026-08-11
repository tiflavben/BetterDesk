/**
 * BetterDesk Console — Phase 4/5 scaffolding routes
 *
 * Phase 4: operator identity profile + consent-popup metadata endpoint
 * Phase 5: agent templates (enrollment presets) + public downloads portal
 *
 * This module is intentionally minimal — it establishes DB shape and HTTP
 * contract so UI + agent-side work can proceed in parallel. The consent popup
 * itself lives in the agent client (Rust) and fetches `/api/bd/operator-info`
 * when a remote session is requested.
 */

'use strict';

const express = require('express');
const router  = express.Router();
const crypto  = require('crypto');
const db      = require('../services/database');
const { requireAuth, requirePermission } = require('../middleware/auth');

// Shared auth.db handle used for agent_templates (kept alongside users)
let _templatesReady = false;
function ensureTemplatesTable() {
    if (_templatesReady) return;
    try {
        const { getAuthDb } = require('../services/database');
        // Best-effort: better-sqlite3 synchronous path.
        const auth = typeof getAuthDb === 'function' ? getAuthDb() : null;
        if (auth && typeof auth.exec === 'function') {
            auth.exec(`CREATE TABLE IF NOT EXISTS agent_templates (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT NOT NULL UNIQUE,
                description TEXT DEFAULT '',
                config_json TEXT NOT NULL DEFAULT '{}',
                enrollment_token TEXT NOT NULL UNIQUE,
                created_by INTEGER,
                created_at TEXT DEFAULT (datetime('now')),
                updated_at TEXT DEFAULT (datetime('now'))
            )`);
        }
    } catch (e) {
        console.warn('[phase4_5] ensureTemplatesTable:', e.message);
    }
    _templatesReady = true;
}

// ────────────────────────────────────────────────────────────────────────────
// Phase 4 — operator profile
// ────────────────────────────────────────────────────────────────────────────

/**
 * GET /api/users/me/profile — current operator's identity profile.
 */
router.get('/api/users/me/profile', requireAuth, async (req, res) => {
    try {
        const uid = req.session.userId;
        const user = await db.getUserById(uid);
        if (!user) return res.status(404).json({ success: false, error: 'not_found' });
        res.json({
            success: true,
            profile: {
                id: user.id,
                username: user.username,
                role: user.role,
                first_name:   user.first_name   || '',
                last_name:    user.last_name    || '',
                email:        user.email        || '',
                phone:        user.phone        || '',
                role_display: user.role_display || '',
                avatar_url:   user.avatar_url   || '',
            },
        });
    } catch (e) {
        res.status(500).json({ success: false, error: e.message });
    }
});

/**
 * PUT /api/users/me/profile — update own identity fields.
 */
router.put('/api/users/me/profile', requireAuth, async (req, res) => {
    try {
        if (typeof db.updateUserProfile !== 'function') {
            return res.status(501).json({ success: false, error: 'not_implemented' });
        }
        const { first_name, last_name, email, phone, role_display, avatar_url } = req.body || {};

        // Lightweight validation. Heavy validation (e.g. email regex) deferred
        // until we decide whether email must be unique / confirmed.
        if (email && String(email).length > 200) {
            return res.status(400).json({ success: false, error: 'email_too_long' });
        }

        await db.updateUserProfile(req.session.userId, {
            first_name, last_name, email, phone, role_display, avatar_url,
        });
        try {
            await db.logAction(req.session.userId, 'profile_updated', 'Updated operator profile', req.ip);
        } catch (_) {}
        res.json({ success: true });
    } catch (e) {
        res.status(500).json({ success: false, error: e.message });
    }
});

/**
 * GET /api/bd/operator-info?session_id=... — consent-popup payload for agent.
 *
 * Agent calls this (with device JWT) right before showing the remote-session
 * consent popup so the end user sees *who* is asking to connect.
 *
 * NOTE: This is a stub — wiring to an actual session/ticket record requires
 * the session broker from Phase 4 proper. For now it resolves the operator
 * from the authenticated JWT's `user_id` claim.
 */
router.get('/api/bd/operator-info', async (req, res) => {
    try {
        const sessionId = String(req.query.session_id || '').slice(0, 128);
        // In real implementation, session_id would map to an in-progress
        // remote request and we'd lookup the requesting operator. For now we
        // require session auth and return the current user's profile.
        if (!req.session || !req.session.userId) {
            return res.status(401).json({ success: false, error: 'unauthorized' });
        }
        const user = await db.getUserById(req.session.userId);
        if (!user) return res.status(404).json({ success: false, error: 'not_found' });
        res.json({
            success: true,
            session_id: sessionId,
            operator: {
                display_name: [user.first_name, user.last_name].filter(Boolean).join(' ') || user.username,
                username: user.username,
                role_display: user.role_display || user.role,
                email: user.email || '',
                phone: user.phone || '',
                avatar_url: user.avatar_url || '',
            },
        });
    } catch (e) {
        res.status(500).json({ success: false, error: e.message });
    }
});

// ────────────────────────────────────────────────────────────────────────────
// Phase 5 — agent templates + enrollment + downloads portal
// ────────────────────────────────────────────────────────────────────────────

/**
 * GET /api/agent-templates — list templates (admin/operator).
 */
router.get('/api/agent-templates', requireAuth, requirePermission('enrollment.manage'), async (req, res) => {
    try {
        ensureTemplatesTable();
        const rows = await db.query
            ? await db.query('SELECT id, name, description, enrollment_token, created_at, updated_at FROM agent_templates ORDER BY id DESC')
            : [];
        res.json({ success: true, templates: rows });
    } catch (e) {
        res.status(500).json({ success: false, error: e.message, templates: [] });
    }
});

/**
 * POST /api/agent-templates — create a new template.
 * Body: { name, description, config }
 */
router.post('/api/agent-templates', requireAuth, requirePermission('enrollment.manage'), async (req, res) => {
    try {
        ensureTemplatesTable();
        const name = String(req.body?.name || '').trim().slice(0, 100);
        if (!name) return res.status(400).json({ success: false, error: 'name_required' });
        const description = String(req.body?.description || '').slice(0, 500);
        const config = req.body?.config && typeof req.body.config === 'object' ? req.body.config : {};
        const token = crypto.randomBytes(24).toString('hex');

        if (!db.run) {
            return res.status(501).json({ success: false, error: 'db_backend_missing_run' });
        }
        await db.run(
            `INSERT INTO agent_templates (name, description, config_json, enrollment_token, created_by)
             VALUES (?, ?, ?, ?, ?)`,
            [name, description, JSON.stringify(config), token, req.session.userId || null]
        );
        try { await db.logAction(req.session.userId, 'template_created', `Created agent template: ${name}`, req.ip); } catch (_) {}
        res.status(201).json({ success: true, enrollment_token: token });
    } catch (e) {
        if (String(e.message).includes('UNIQUE')) {
            return res.status(409).json({ success: false, error: 'name_exists' });
        }
        res.status(500).json({ success: false, error: e.message });
    }
});

/**
 * DELETE /api/agent-templates/:id
 */
router.delete('/api/agent-templates/:id', requireAuth, requirePermission('enrollment.manage'), async (req, res) => {
    try {
        const id = parseInt(req.params.id, 10);
        if (!Number.isFinite(id) || id <= 0) {
            return res.status(400).json({ success: false, error: 'invalid_id' });
        }
        if (!db.run) return res.status(501).json({ success: false, error: 'db_backend_missing_run' });
        await db.run('DELETE FROM agent_templates WHERE id = ?', [id]);
        res.json({ success: true });
    } catch (e) {
        res.status(500).json({ success: false, error: e.message });
    }
});

/**
 * POST /api/bd/enroll — agent enrollment using a template token.
 * Body: { enrollment_token, device_id, device_name, sysinfo }
 *
 * This endpoint is exposed publicly (no auth) — security comes from the
 * one-use enrollment_token in the template. Once enrolled the agent obtains
 * a normal device JWT via the existing /api/bd/register flow.
 */
router.post('/api/bd/enroll', async (req, res) => {
    try {
        ensureTemplatesTable();
        const token = String(req.body?.enrollment_token || '').slice(0, 64);
        const deviceId = String(req.body?.device_id || '').slice(0, 64);
        if (!token || !deviceId) {
            return res.status(400).json({ success: false, error: 'missing_fields' });
        }

        // The agent_templates table lives on the auth DB handle (see
        // ensureTemplatesTable). Prefer the synchronous better-sqlite3 path;
        // keep the legacy db.get/db.run fallback for other backends.
        const { getAuthDb } = require('../services/database');
        const auth = (typeof getAuthDb === 'function') ? getAuthDb() : null;

        let row = null;
        if (auth && typeof auth.prepare === 'function') {
            row = auth.prepare('SELECT id, name, config_json, enrollment_token FROM agent_templates WHERE enrollment_token = ?').get(token) || null;
        } else if (db.get) {
            row = await db.get('SELECT id, name, config_json, enrollment_token FROM agent_templates WHERE enrollment_token = ?', [token]) || null;
        }
        if (!row) return res.status(401).json({ success: false, error: 'invalid_token' });

        // One-use token: consume it atomically BEFORE returning the config.
        // The token column is NOT NULL UNIQUE with no used/status column, so
        // "used" is recorded by rotating the value to a random used- marker.
        // The WHERE enrollment_token = ? clause wins the race between two
        // concurrent enrolls with the same token (second one matches 0 rows).
        let consumed = false;
        if (auth && typeof auth.prepare === 'function') {
            const r = auth.prepare(
                "UPDATE agent_templates SET enrollment_token = 'used-' || lower(hex(randomblob(16))), updated_at = datetime('now') WHERE id = ? AND enrollment_token = ?"
            ).run(row.id, token);
            consumed = r.changes > 0;
        } else if (db.run) {
            await db.run(
                "UPDATE agent_templates SET enrollment_token = ? WHERE id = ? AND enrollment_token = ?",
                ['used-' + crypto.randomBytes(16).toString('hex'), row.id, token]
            );
            consumed = true; // generic backend: best-effort
        }
        if (!consumed) return res.status(401).json({ success: false, error: 'invalid_token' });

        let config = {};
        try { config = JSON.parse(row.config_json || '{}'); } catch (_) {}

        // Audit trail. Actual device creation is handled by agent's subsequent
        // /api/bd/register call which uses the returned `preset_config` to seed
        // capabilities, tags, group membership, etc.
        try { await db.logAction(null, 'device_enrolled', `Device ${deviceId} enrolled via template "${row.name}"`, req.ip); } catch (_) {}

        res.json({
            success: true,
            template_name: row.name,
            preset_config: config,
        });
    } catch (e) {
        res.status(500).json({ success: false, error: e.message });
    }
});

/**
 * GET /portal — branded public download page.
 * No auth required. Lists available installers + enrollment instructions.
 */
router.get('/portal', (req, res) => {
    res.render('downloads-portal', {
        title: req.t ? req.t('portal.title') : 'Download BetterDesk',
        layout: false, // standalone page
    });
});

/**
 * GET /api/portal/installers — list available installers (JSON).
 * The actual binaries are served from /downloads/ (static). This endpoint
 * just enumerates metadata.
 */
router.get('/api/portal/installers', (req, res) => {
    const base = req.protocol + '://' + req.get('host');
    res.json({
        success: true,
        installers: [
            { platform: 'windows', arch: 'x64', url: `${base}/downloads/BetterDesk_Agent_x64-setup.exe`, format: 'nsis' },
            { platform: 'linux',   arch: 'x64', url: `${base}/downloads/betterdesk-agent-linux-amd64`,   format: 'binary' },
            { platform: 'linux',   arch: 'arm64', url: `${base}/downloads/betterdesk-agent-linux-arm64`, format: 'binary' },
        ],
    });
});

module.exports = router;
