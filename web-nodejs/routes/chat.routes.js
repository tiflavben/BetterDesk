/**
 * BetterDesk Console — Chat Routes
 * Dedicated chat page + API endpoints for Chat 2.0
 */

'use strict';

const express = require('express');
const router = express.Router();
const multer = require('multer');
const path = require('path');
const fs = require('fs');
const crypto = require('crypto');
const { requireAuth, requirePermission } = require('../middleware/auth');

// File upload config — max 50MB, store in data/chat-files/<userId>/
const UPLOAD_DIR = path.join(__dirname, '..', 'data', 'chat-files');
const MAX_FILE_SIZE = 50 * 1024 * 1024; // 50MB
const MAX_USER_QUOTA_BYTES = 100 * 1024 * 1024; // 100MB per user (cumulative, persistent)

// Per-user quota is derived from the filesystem: each uploader gets a
// subdirectory under UPLOAD_DIR and the quota is the summed size of the files
// in it. This survives restarts without a DB table (no chat_attachments table
// exists yet). Upgrade path: introduce a chat_attachments(size, uploaded_by)
// table and switch the accounting to SELECT SUM(size) WHERE uploaded_by = ?.
const userUploadBytes = null; // (legacy in-memory map removed — see getUserUploadBytes)

/** Resolve the uploader identity exactly as the route handler does. */
function uploaderUid(req) {
    return String(req.session?.userId ?? req.session?.user?.id ?? req.ip);
}

/** Map an arbitrary session identity to a safe per-user subdirectory. */
function userUploadDir(uid) {
    const safe = String(uid).replace(/[^A-Za-z0-9_-]+/g, '_').slice(0, 64) || '_';
    const dir = path.join(UPLOAD_DIR, safe);
    // Path traversal guard (defense in depth — `safe` is already sanitized).
    if (!dir.startsWith(UPLOAD_DIR)) throw new Error('Invalid upload directory');
    return dir;
}

/** Recursively sum the sizes of regular files under a directory. */
function dirBytes(dir) {
    let total = 0;
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isFile()) {
            try { total += fs.statSync(full).size; } catch (_) { /* raced deletion */ }
        } else if (entry.isDirectory()) {
            total += dirBytes(full);
        }
    }
    return total;
}

/** Total bytes currently stored for a user (0 when the user has no uploads). */
function getUserUploadBytes(uid) {
    const dir = userUploadDir(uid);
    return fs.existsSync(dir) ? dirBytes(dir) : 0;
}

/** Find a stored file by its random-id prefix, searching per-user subdirs and legacy flat files. */
function findFileById(dir, fileId) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isFile()) {
            if (entry.name.startsWith(fileId)) return full;
        } else if (entry.isDirectory()) {
            const found = findFileById(full, fileId);
            if (found) return found;
        }
    }
    return null;
}

if (!fs.existsSync(UPLOAD_DIR)) {
    fs.mkdirSync(UPLOAD_DIR, { recursive: true });
}

const upload = multer({
    storage: multer.diskStorage({
        destination: (req, _file, cb) => {
            // Per-user subdirectory; created on demand (diskStorage does not mkdir).
            const dir = userUploadDir(uploaderUid(req));
            try {
                if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });
                cb(null, dir);
            } catch (err) {
                cb(err);
            }
        },
        filename: (_req, file, cb) => {
            const id = crypto.randomBytes(16).toString('hex');
            const ext = path.extname(file.originalname).slice(0, 10);
            cb(null, `${id}${ext}`);
        },
    }),
    limits: { fileSize: MAX_FILE_SIZE },
});

// ========== Page Route ==========

router.get('/chat', requireAuth, (req, res) => {
    res.render('chat', {
        title: req.t('nav.chat'),
        pageStyles: ['chat'],
        pageScripts: ['chat'],
        currentPage: 'chat',
        breadcrumb: [{ label: req.t('nav.chat') }],
    });
});

// ========== API Routes ==========

// Upload encrypted file
router.post('/api/chat/upload', requireAuth, upload.single('file'), (req, res) => {
    if (!req.file) {
        return res.status(400).json({ success: false, error: 'No file provided' });
    }
    const uid = uploaderUid(req);
    const used = getUserUploadBytes(uid);
    if (used + req.file.size > MAX_USER_QUOTA_BYTES) {
        // Quota exceeded — remove the already-written file and reject.
        try { fs.unlinkSync(req.file.path); } catch (_) { /* best-effort cleanup */ }
        return res.status(413).json({ success: false, error: 'upload_quota_exceeded' });
    }
    const fileId = path.basename(req.file.filename, path.extname(req.file.filename));
    res.json({
        success: true,
        file_id: fileId,
        filename: req.file.filename,
        size: req.file.size,
        url: `/api/chat/files/${fileId}`,
    });
});

// Download file
router.get('/api/chat/files/:fileId', requireAuth, requirePermission('chat.access'), (req, res) => {
    const fileId = req.params.fileId.replace(/[^a-f0-9]/gi, '');
    if (!fileId) return res.status(400).json({ success: false, error: 'Invalid file ID' });

    // Find file by ID prefix (recursive: per-user subdirs + legacy flat files)
    const filePath = findFileById(UPLOAD_DIR, fileId);
    if (!filePath) {
        return res.status(404).json({ success: false, error: 'File not found' });
    }
    // Verify path traversal protection
    if (!filePath.startsWith(UPLOAD_DIR)) {
        return res.status(403).json({ success: false, error: 'Forbidden' });
    }
    res.download(filePath);
});

// Search messages via Go server proxy
router.get('/api/chat/search', requireAuth, async (req, res) => {
    try {
        const { apiClient } = require('../services/betterdeskApi');
        const query = String(req.query.q || '').slice(0, 200);
        const conversationId = req.query.conversation_id || '';
        if (!query) return res.json({ success: true, messages: [] });

        const resp = await apiClient.get('/api/chat/search', {
            params: { q: query, conversation_id: conversationId, limit: 50 },
        });
        res.json({ success: true, messages: resp.data?.messages || [] });
    } catch (e) {
        res.json({ success: true, messages: [] });
    }
});

module.exports = router;
