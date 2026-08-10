/**
 * BetterDesk Console - Inventory Page Script
 */

(function () {
    'use strict';

    const _ = window.BetterDesk?.translations
        ? (key) => {
            const keys = key.split('.');
            let val = window.BetterDesk.translations;
            for (const k of keys) { val = val?.[k]; }
            return val || key;
        }
        : (key) => key;

    const csrfToken = window.BetterDesk?.csrfToken || '';

    // State
    let allDevices = [];
    let currentSearch = '';

    // DOM refs
    const tbody = document.getElementById('inventory-body');
    const searchInput = document.getElementById('search-input');
    const refreshBtn = document.getElementById('refresh-btn');
    const detailModal = document.getElementById('detail-modal');
    const detailBody = document.getElementById('detail-modal-body');
    const statDevices = document.getElementById('stat-devices');
    const statCpu = document.getElementById('stat-cpu');
    const statMemory = document.getElementById('stat-memory');
    const statDisks = document.getElementById('stat-disks');

    // ---- API helpers ----

    async function apiFetch(url, options = {}) {
        const headers = { 'Content-Type': 'application/json' };
        if (csrfToken) headers['x-csrf-token'] = csrfToken;
        const resp = await fetch(url, { ...options, headers: { ...headers, ...options.headers } });
        return resp.json();
    }

    // ---- Load ----

    async function loadInventory() {
        tbody.innerHTML = `<tr class="loading-row"><td colspan="8">${_('common.loading')}</td></tr>`;

        try {
            const result = await apiFetch('/api/inventory');
            allDevices = result.devices || [];
            updateStats();
            renderTable();
        } catch (err) {
            tbody.innerHTML = `
                <tr class="empty-row"><td colspan="8">
                    <div class="empty-state">
                        <span class="material-icons">error_outline</span>
                        <p>${escapeHtml(err.message || _('common.error'))}</p>
                    </div>
                </td></tr>`;
        }
    }

    function updateStats() {
        statDevices.textContent = allDevices.length;
        const avgCpu = allDevices.reduce((s, d) => s + (d.cpu_usage || 0), 0) / (allDevices.length || 1);
        const avgMem = allDevices.reduce((s, d) => s + (d.memory_used_mb || 0), 0);
        const totalDisks = allDevices.reduce((s, d) => s + (d.disk_count || 0), 0);
        statCpu.textContent = avgCpu > 0 ? avgCpu.toFixed(1) + '%' : '—';
        statMemory.textContent = avgMem > 0 ? formatBytes(avgMem * 1024 * 1024) : '—';
        statDisks.textContent = totalDisks || '—';
    }

    // ---- Render ----

    function renderTable() {
        const filtered = allDevices.filter(d => {
            if (!currentSearch) return true;
            const q = currentSearch.toLowerCase();
            return (d.device_id || '').toLowerCase().includes(q)
                || (d.hostname || '').toLowerCase().includes(q)
                || (d.os || '').toLowerCase().includes(q)
                || (d.cpu || '').toLowerCase().includes(q);
        });

        if (!filtered.length) {
            tbody.innerHTML = `
                <tr class="empty-row"><td colspan="8">
                    <div class="empty-state">
                        <span class="material-icons">inventory_2</span>
                        <p>${_('inventory.no_devices')}</p>
                    </div>
                </td></tr>`;
            return;
        }

        tbody.innerHTML = filtered.map(d => `
            <tr data-id="${escapeHtml(d.device_id)}">
                <td class="device-id-cell">${escapeHtml(d.device_id)}</td>
                <td>${escapeHtml(d.hostname || '—')}</td>
                <td>${escapeHtml(d.os || '—')}</td>
                <td title="${escapeHtml(d.cpu || '')}">${truncate(d.cpu || '—', 30)}</td>
                <td>${formatMemory(d.memory_total_mb, d.memory_used_mb)}</td>
                <td>${d.disk_count != null ? d.disk_count : '—'}</td>
                <td class="time-cell" title="${escapeHtml(d.collected_at || d.last_seen || '')}">${formatTimeAgo(d.collected_at || d.last_seen)}</td>
                <td>
                    <button class="btn btn-sm btn-secondary view-detail-btn" data-id="${escapeHtml(d.device_id)}">
                        <span class="material-icons" style="font-size:16px">visibility</span>
                    </button>
                </td>
            </tr>
        `).join('');

        tbody.querySelectorAll('.view-detail-btn').forEach(btn => {
            btn.addEventListener('click', () => openDetail(btn.dataset.id));
        });
    }

    // ---- Detail ----

    async function openDetail(deviceId) {
        detailModal.style.display = 'flex';
        detailBody.innerHTML = `<p>${_('common.loading')}</p>`;

        try {
            const data = await apiFetch(`/api/inventory/${encodeURIComponent(deviceId)}`);
            renderDetail(data);
        } catch (err) {
            detailBody.innerHTML = `<p class="text-error">${escapeHtml(err.message)}</p>`;
        }
    }

    function renderDetail(data) {
        const tel = data.telemetry || {};
        const cpuUsage = tel.cpu_usage != null ? tel.cpu_usage : data.cpu_usage;
        const memPct = data.memory_total_mb > 0
            ? Math.round((data.memory_used_mb / data.memory_total_mb) * 100) : null;

        let html = `
            <div class="detail-section">
                <h4>${_('inventory.system')}</h4>
                <div class="detail-grid">
                    <div class="detail-item"><span class="detail-label">${_('inventory.device_id')}</span><span class="detail-value">${escapeHtml(data.device_id)}</span></div>
                    <div class="detail-item"><span class="detail-label">${_('inventory.hostname')}</span><span class="detail-value">${escapeHtml(data.hostname || '—')}</span></div>
                    <div class="detail-item"><span class="detail-label">${_('inventory.os')}</span><span class="detail-value">${escapeHtml(data.os || '—')}</span></div>
                    <div class="detail-item"><span class="detail-label">${_('inventory.architecture')}</span><span class="detail-value">${escapeHtml(data.architecture || '—')}</span></div>
                    <div class="detail-item"><span class="detail-label">${_('inventory.kernel')}</span><span class="detail-value">${escapeHtml(data.kernel || '—')}</span></div>
                    <div class="detail-item"><span class="detail-label">${_('inventory.uptime')}</span><span class="detail-value">${formatDuration(data.uptime || tel.uptime)}</span></div>
                </div>
            </div>
            <div class="detail-section">
                <h4>${_('inventory.cpu')}</h4>
                <div class="detail-grid">
                    <div class="detail-item"><span class="detail-label">${_('inventory.model')}</span><span class="detail-value">${escapeHtml(data.cpu || '—')}</span></div>
                    <div class="detail-item"><span class="detail-label">${_('inventory.cpu_cores')}</span><span class="detail-value">${data.cpu_cores || '—'}</span></div>
                </div>`;

        if (cpuUsage != null) {
            const cls = cpuUsage > 90 ? 'danger' : cpuUsage > 70 ? 'warning' : '';
            html += `<div style="margin-top:8px">
                <span class="detail-label">${_('inventory.cpu_usage')}: ${cpuUsage.toFixed(1)}%</span>
                <div class="usage-bar"><div class="usage-bar-fill ${cls}" style="width:${cpuUsage}%"></div></div>
            </div>`;
        }
        html += `</div>`;

        html += `<div class="detail-section">
            <h4>${_('inventory.memory')}</h4>
            <div class="detail-grid">
                <div class="detail-item"><span class="detail-label">${_('inventory.total')}</span><span class="detail-value">${formatBytes((data.memory_total_mb || 0) * 1024 * 1024)}</span></div>
                <div class="detail-item"><span class="detail-label">${_('inventory.used')}</span><span class="detail-value">${formatBytes((data.memory_used_mb || 0) * 1024 * 1024)}</span></div>
            </div>`;

        if (memPct != null) {
            const cls = memPct > 90 ? 'danger' : memPct > 70 ? 'warning' : '';
            html += `<div style="margin-top:8px">
                <span class="detail-label">${_('inventory.memory_usage')}: ${memPct}%</span>
                <div class="usage-bar"><div class="usage-bar-fill ${cls}" style="width:${memPct}%"></div></div>
            </div>`;
        }
        html += `</div>`;

        if (data.disks && data.disks.length) {
            html += `<div class="detail-section"><h4>${_('inventory.disks')}</h4>`;
            data.disks.forEach(disk => {
                const pct = disk.total_gb > 0 ? Math.round((disk.used_gb / disk.total_gb) * 100) : 0;
                const cls = pct > 90 ? 'danger' : pct > 70 ? 'warning' : '';
                html += `<div style="margin-bottom:8px">
                    <span class="detail-label">${escapeHtml(disk.mount_point || disk.device || '?')}: ${pct}% (${disk.used_gb?.toFixed(1) || 0}/${disk.total_gb?.toFixed(1) || 0} GB)</span>
                    <div class="usage-bar"><div class="usage-bar-fill ${cls}" style="width:${pct}%"></div></div>
                </div>`;
            });
            html += `</div>`;
        }

        if (data.network_interfaces && data.network_interfaces.length) {
            html += `<div class="detail-section"><h4>${_('inventory.network')}</h4>`;
            data.network_interfaces.forEach(nic => {
                html += `<div style="margin-bottom:6px">
                    <span class="detail-value">${escapeHtml(nic.name || '?')}</span>
                    <span class="detail-label" style="margin-left:8px">${escapeHtml(nic.mac || '')} — ${escapeHtml((nic.ip_addresses || []).join(', '))}</span>
                </div>`;
            });
            html += `</div>`;
        }

        detailBody.innerHTML = html;
    }

    // ---- Helpers ----

    function escapeHtml(str) {
        if (!str) return '';
        const div = document.createElement('div');
        div.textContent = String(str);
        return div.innerHTML;
    }

    function truncate(str, max) {
        if (!str) return str;
        return str.length > max ? str.slice(0, max) + '…' : str;
    }

    function formatBytes(bytes) {
        if (!bytes || bytes <= 0) return '—';
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        let i = 0;
        let val = bytes;
        while (val >= 1024 && i < units.length - 1) { val /= 1024; i++; }
        return val.toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    }

    function formatMemory(total, used) {
        if (!total || used == null) return '—';
        const pct = Math.round((used / total) * 100);
        return `${formatBytes(used * 1024 * 1024)} / ${formatBytes(total * 1024 * 1024)} (${pct}%)`;
    }

    function formatDuration(seconds) {
        if (!seconds) return '—';
        const d = Math.floor(seconds / 86400);
        const h = Math.floor((seconds % 86400) / 3600);
        const m = Math.floor((seconds % 3600) / 60);
        const parts = [];
        if (d) parts.push(d + 'd');
        if (h) parts.push(h + 'h');
        if (m || !parts.length) parts.push(m + 'm');
        return parts.join(' ');
    }

    function formatTimeAgo(dateStr) {
        if (!dateStr) return '—';
        const d = new Date(dateStr);
        if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
        const diff = Math.floor((Date.now() - d) / 1000);
        if (diff < 60) return _('time.seconds_ago').replace('{count}', diff);
        if (diff < 3600) return _('time.minutes_ago').replace('{count}', Math.floor(diff / 60));
        if (diff < 86400) return _('time.hours_ago').replace('{count}', Math.floor(diff / 3600));
        if (diff < 2592000) return _('time.days_ago').replace('{count}', Math.floor(diff / 86400));
        return d.toLocaleDateString();
    }

    function showToast(message, type) {
        if (window.BetterDesk?.notify) { window.BetterDesk.notify(message, type); return; }
        const container = document.getElementById('toast-container');
        if (!container) return;
        const toast = document.createElement('div');
        toast.className = `toast toast-${type || 'info'}`;
        toast.textContent = message;
        container.appendChild(toast);
        setTimeout(() => toast.remove(), 4000);
    }

    // ---- Events ----

    let searchTimeout;
    searchInput.addEventListener('input', () => {
        clearTimeout(searchTimeout);
        searchTimeout = setTimeout(() => {
            currentSearch = searchInput.value.trim();
            renderTable();
        }, 300);
    });

    refreshBtn.addEventListener('click', loadInventory);

    document.querySelectorAll('.modal-close').forEach(btn => {
        btn.addEventListener('click', () => {
            const modalId = btn.dataset.modal;
            if (modalId) document.getElementById(modalId).style.display = 'none';
        });
    });

    // ---- Init ----
    loadInventory();
})();
