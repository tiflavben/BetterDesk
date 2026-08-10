/**
 * BetterDesk Console - CDAP Routes
 * Routes for CDAP (Custom Device Automation Protocol) device management
 * and widget rendering in the admin panel.
 */

const express = require('express');
const router = express.Router();
const { assertSafeApiId } = require('../lib/goApiPath');
const { requireAuth, requirePermission, requireAdmin } = require('../middleware/auth');
const betterdeskApi = require('../services/betterdeskApi');

// ── Page Routes ──────────────────────────────────────────────────────────

/**
 * CDAP devices list page
 * GET /cdap
 */
router.get('/cdap', requireAuth, requireAdmin, (req, res) => {
    res.render('cdap-devices', {
        title: req.t('cdap.devices_title'),
        activePage: 'cdap',
        currentPage: 'cdap'
    });
});

/**
 * CDAP devices list page (alternative path, used by desktop embed)
 * GET /cdap/devices
 */
router.get('/cdap/devices', requireAuth, requireAdmin, (req, res) => {
    res.render('cdap-devices', {
        title: req.t('cdap.devices_title'),
        activePage: 'cdap',
        currentPage: 'cdap'
    });
});

/**
 * CDAP device detail page with widget panel
 * GET /cdap/devices/:id
 */
router.get('/cdap/devices/:id', requireAuth, requireAdmin, (req, res) => {
    try {
        const deviceId = assertSafeApiId(req.params.id, 'deviceId');
        res.render('cdap-device', {
            title: req.t('cdap.device_detail'),
            activePage: 'devices',
            deviceId,
        });
    } catch (err) {
        if (err.message && /^Invalid /.test(err.message)) {
            return res.status(400).render('errors/404', {
                title: 'Bad Request',
                activePage: 'error',
            });
        }
        throw err;
    }
});

// ── API Routes ───────────────────────────────────────────────────────────

// Unwrap the { success, data } envelope returned by betterdeskApi helpers
// so that the CDAP frontend receives the flat Go server response it expects.
function unwrap(result) {
    return (result && result.success && result.data) ? result.data : result;
}

/**
 * GET /api/cdap/status
 * Returns CDAP gateway status (enabled, connections, port)
 */
router.get('/api/cdap/status', requireAuth, requirePermission('cdap.view'), async (req, res) => {
    try {
        const result = await betterdeskApi.getCDAPStatus();
        res.json(unwrap(result));
    } catch (err) {
        const status = err.response?.status || 500;
        const error = (err.response && err.response.data && (err.response.data.error || err.response.data.message))
            || err.message || 'Failed to get CDAP status';
        res.status(status).json({ enabled: false, error });
    }
});

/**
 * GET /api/cdap/devices
 * Returns all connected CDAP devices
 */
router.get('/api/cdap/devices', requireAuth, requirePermission('cdap.view'), async (req, res) => {
    try {
        const result = await betterdeskApi.getCDAPDevices();
        res.json(unwrap(result));
    } catch (err) {
        const status = err.response?.status || 500;
        const error = (err.response && err.response.data && (err.response.data.error || err.response.data.message))
            || err.message || 'Failed to list CDAP devices';
        res.status(status).json({ devices: [], error });
    }
});

/**
 * GET /api/cdap/devices/:id
 * Returns full CDAP device info (manifest + state + connection)
 */
router.get('/api/cdap/devices/:id', requireAuth, requirePermission('cdap.view'), async (req, res) => {
    try {
        const result = await betterdeskApi.getCDAPDeviceInfo(req.params.id);
        res.json(unwrap(result));
    } catch (err) {
        res.status(500).json({ error: 'Failed to get CDAP device info' });
    }
});

/**
 * GET /api/cdap/devices/:id/manifest
 * Returns device manifest (capabilities, widgets, alerts)
 */
router.get('/api/cdap/devices/:id/manifest', requireAuth, requirePermission('cdap.view'), async (req, res) => {
    try {
        const result = await betterdeskApi.getCDAPDeviceManifest(req.params.id);
        res.json(unwrap(result));
    } catch (err) {
        res.status(500).json({ error: 'Failed to get CDAP device manifest' });
    }
});

/**
 * GET /api/cdap/devices/:id/state
 * Returns current widget values for connected device
 */
router.get('/api/cdap/devices/:id/state', requireAuth, requirePermission('cdap.view'), async (req, res) => {
    try {
        const result = await betterdeskApi.getCDAPDeviceState(req.params.id);
        res.json(unwrap(result));
    } catch (err) {
        res.status(500).json({ error: 'Failed to get CDAP device state' });
    }
});

/**
 * POST /api/cdap/devices/:id/command
 * Sends a command to a connected CDAP device
 * Body: { widget_id, action, value, reason? }
 */
router.post('/api/cdap/devices/:id/command', requireAuth, requirePermission('cdap.command'), async (req, res) => {
    try {
        // Defense in depth: reject commands for unknown CDAP devices.
        const device = await betterdeskApi.getCDAPDeviceInfo(req.params.id);
        if (!device || (device.success === false && device.error)) {
            return res.status(404).json({ error: 'CDAP device not found' });
        }
        const { widget_id, action, value, reason } = req.body;

        if (!widget_id || !action) {
            return res.status(400).json({ error: 'widget_id and action are required' });
        }

        const result = await betterdeskApi.sendCDAPCommand(
            req.params.id,
            widget_id,
            action,
            value,
            reason
        );
        res.json(unwrap(result));
    } catch (err) {
        res.status(500).json({ error: 'Failed to send command' });
    }
});

/**
 * POST /api/cdap/toggle
 * Enable or disable CDAP gateway (saves config; requires server restart)
 * Body: { enabled: true|false }
 */
router.post('/api/cdap/toggle', requireAuth, requirePermission('server.config'), async (req, res) => {
    try {
        const { enabled } = req.body;
        if (typeof enabled !== 'boolean') {
            return res.status(400).json({ success: false, error: 'enabled must be a boolean' });
        }
        await betterdeskApi.setConfig('cdap_enabled', enabled ? 'Y' : 'N');
        res.json({ success: true, enabled, restart_required: true });
    } catch (err) {
        console.error('[CDAP] Toggle error:', err.message);
        res.status(500).json({ success: false, error: 'Failed to toggle CDAP' });
    }
});

/**
 * GET /api/cdap/alerts
 * Returns all currently firing CDAP alerts
 * Query: ?device_id=optional (filter by device)
 */
router.get('/api/cdap/alerts', requireAuth, requirePermission('cdap.view'), async (req, res) => {
    try {
        const result = await betterdeskApi.getCDAPAlerts(req.query.device_id);
        res.json(unwrap(result));
    } catch (err) {
        res.status(500).json({ alerts: [], total: 0, error: 'Failed to get CDAP alerts' });
    }
});

/**
 * GET /api/cdap/devices/:id/linked
 * Returns all peers linked to this CDAP device
 */
router.get('/api/cdap/devices/:id/linked', requireAuth, requirePermission('cdap.view'), async (req, res) => {
    try {
        const result = await betterdeskApi.getLinkedPeers(req.params.id);
        res.json(unwrap(result));
    } catch (err) {
        res.status(500).json({ error: 'Failed to get linked devices' });
    }
});

/**
 * POST /api/cdap/devices/:id/link
 * Link or unlink a peer to this CDAP device
 * Body: { linked_peer_id: "PEERID" } or { linked_peer_id: "" } to unlink
 */
router.post('/api/cdap/devices/:id/link', requireAuth, requirePermission('cdap.command'), async (req, res) => {
    try {
        // Defense in depth: reject commands for unknown CDAP devices.
        const device = await betterdeskApi.getCDAPDeviceInfo(req.params.id);
        if (!device || (device.success === false && device.error)) {
            return res.status(404).json({ error: 'CDAP device not found' });
        }
        const { linked_peer_id } = req.body;
        if (linked_peer_id === undefined) {
            return res.status(400).json({ error: 'linked_peer_id is required' });
        }
        const result = await betterdeskApi.linkDevice(req.params.id, linked_peer_id);
        res.json(unwrap(result));
    } catch (err) {
        res.status(500).json({ error: 'Failed to link device' });
    }
});

module.exports = router;
