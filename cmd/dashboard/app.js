// escapeHtml prevents XSS by escaping user-controlled strings before
// inserting them into innerHTML template literals.
function escapeHtml(str) {
    if (str == null) return '';
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}

let previousActiveElement;
let modalKeydownHandler;

// ── Auth (G-1, G-2) ─────────────────────────────────────────────────────────

function getToken() {
    return sessionStorage.getItem('aip-token') || '';
}

function setToken(t) {
    if (t) sessionStorage.setItem('aip-token', t);
    else sessionStorage.removeItem('aip-token');
}

// apiFetch wraps fetch() injecting Authorization and handling 401/403.
async function apiFetch(url, opts = {}) {
    const token = getToken();
    const headers = { ...(opts.headers || {}) };
    if (token) headers['Authorization'] = 'Bearer ' + token;

    let resp;
    try {
        resp = await fetch(url, { ...opts, headers });
    } catch (err) {
        if (state.proxyAuth) {
            // Cross-origin redirect from oauth2-proxy can cause fetch to throw.
            // Apply the same reload cap used for 401/403 to avoid infinite loops.
            const count = parseInt(sessionStorage.getItem('reload-count') || '0', 10);
            if (count < 3) {
                sessionStorage.setItem('reload-count', (count + 1).toString());
                window.location.reload();
            } else {
                showBanner('Session expired. Authentication redirect failed.', 'error');
            }
            return new Response(JSON.stringify({ error: 'Session expired' }), {
                status: 401,
                headers: { 'Content-Type': 'application/json' },
            });
        }
        throw err;
    }

    // 401 always means unauthenticated. 403 from oauth2-proxy means session
    // expired (AJAX calls get 403, not 302) — oauth2-proxy returns text/html
    // (the login page); backend permission denials return application/json.
    // Distinguish them by Content-Type so a real backend 403 shows a banner
    // instead of incorrectly triggering a reload loop.
    const is403ProxyExpiry = resp.status === 403 && state.proxyAuth &&
        (resp.headers.get('content-type') || '').includes('text/html');
    const isSessionExpiry = resp.status === 401 ||
        is403ProxyExpiry ||
        (state.proxyAuth && resp.redirected);

    if (isSessionExpiry) {
        if (state.proxyAuth) {
            const count = parseInt(sessionStorage.getItem('reload-count') || '0', 10);
            if (count < 3) {
                sessionStorage.setItem('reload-count', (count + 1).toString());
                window.location.reload();
            } else {
                showBanner('Session expired. Authentication redirect failed.', 'error');
            }
            // Return a synthetic Response so callers can safely call .json()/.text()
            return new Response(JSON.stringify({ error: 'Session expired' }), {
                status: 401,
                statusText: 'Session expired',
                headers: { 'Content-Type': 'application/json' },
            });
        }
        showBanner('Session expired — please re-enter your token.', 'error');
    } else if (resp.status === 403) {
        showBanner('Permission denied — check token or access rights.', 'error');
    } else if (state.proxyAuth) {
        sessionStorage.removeItem('reload-count');
    }
    return resp;
}

function showBanner(msg, type) {
    const el = document.getElementById('auth-banner');
    if (!el) return;
    el.style.display = 'block';
    el.textContent = msg;
    if (type === 'error') {
        el.style.color = 'var(--error)';
        el.style.borderColor = 'var(--error)';
        el.style.background = 'rgba(239,68,68,0.08)';
    } else if (type === 'warn') {
        el.style.color = 'var(--warning)';
        el.style.borderColor = 'var(--warning)';
        el.style.background = 'rgba(245,158,11,0.08)';
    } else {
        el.style.color = 'var(--success)';
        el.style.borderColor = 'var(--success)';
        el.style.background = 'rgba(34,197,94,0.08)';
    }
}

function hideBanner() {
    const el = document.getElementById('auth-banner');
    if (el) el.style.display = 'none';
}

window.toggleTokenPanel = function() {
    const panel = document.getElementById('token-panel');
    const showing = panel.style.display !== 'none';
    panel.style.display = showing ? 'none' : 'block';
    if (!showing) {
        const inp = document.getElementById('token-input');
        inp.value = getToken();
        inp.focus();
    }
};

window.applyToken = async function() {
    const inp = document.getElementById('token-input');
    const token = inp.value.trim();
    setToken(token);
    document.getElementById('token-panel').style.display = 'none';
    if (token) {
        hideBanner();
        await loadIdentity();
    } else {
        showBanner('Not authenticated — paste a Bearer token to continue.', 'warn');
        updateRoleUI('');
    }
    fetchRequests();
};

window.clearToken = function() {
    document.getElementById('token-input').value = '';
    setToken('');
    document.getElementById('token-panel').style.display = 'none';
    showBanner('Not authenticated — paste a Bearer token to continue.', 'warn');
    updateRoleUI('');
};

// ── Identity / role (G-3) ────────────────────────────────────────────────────

const state = {
    requests: [],
    selectedRequest: null,
    auditRecords: [],
    diagnostics: [],
    diagnosticsJSON: '',
    namespace: 'default',
    diagnosticsGen: 0,
    requestsGen: 0,
    role: '',          // 'agent' | 'reviewer' | 'admin' | ''
    identity: '',
    proxyAuth: false,  // true when auth proxy is injecting credentials
};

async function loadIdentity() {
    try {
        const resp = await apiFetch('/api/whoami');
        if (!resp.ok) {
            if (!getToken()) updateRoleUI('');
            return;
        }
        const data = await resp.json();
        state.identity = data.identity || '';
        updateRoleUI(data.role || '');
        // If whoami succeeded without a manually stored token, the auth proxy
        // is injecting credentials — set proxyAuth and hide the token UI.
        if (!getToken() && data.identity && data.identity !== 'unknown') {
            state.proxyAuth = true;
            const btn = document.getElementById('token-btn');
            if (btn) btn.style.display = 'none';
            hideBanner();
        }
    } catch (_) {
        if (!getToken()) updateRoleUI('');
    }
}

function updateRoleUI(role) {
    state.role = role;

    const chip = document.getElementById('identity-chip');
    if (chip) {
        if (state.identity && role) {
            chip.style.display = 'inline-block';
            chip.textContent = state.identity + ' (' + role + ')';
        } else {
            chip.style.display = 'none';
        }
    }

    // Admin-only tabs
    const adminTabIds = ['tab-governed-resources', 'tab-safety-policies'];
    for (const id of adminTabIds) {
        const tab = document.getElementById(id);
        if (tab) tab.style.display = role === 'admin' ? 'inline-block' : 'none';
    }
}

// ── Agent Requests tab ───────────────────────────────────────────────────────

async function fetchRequests() {
    if (!getToken() && !state.proxyAuth) {
        showBanner('Not authenticated — paste a Bearer token to continue.', 'warn');
        return;
    }
    const ns = document.getElementById('req-ns-input')?.value?.trim() || 'default';
    try {
        const response = await apiFetch(`/api/agent-requests?namespace=${encodeURIComponent(ns)}`);
        if (!response.ok) {
            if (response.status !== 401) {
                showBanner('Failed to load requests (HTTP ' + response.status + ').', 'error');
            }
            return;
        }
        hideBanner();
        const reqGen = ++state.requestsGen;
        fetchRequestSummaries(reqGen, ns);

        const data = await response.json();
        state.requests = data.sort((a, b) => new Date(b.metadata.creationTimestamp) - new Date(a.metadata.creationTimestamp));
        renderList();

        if (state.requests.length === 0) {
            state.selectedRequest = null;
            state.auditRecords = [];
            renderDetails();
        } else if (!state.selectedRequest) {
            selectRequest(state.requests[0]);
        } else {
            const stillExists = state.requests.find(r => r.metadata.name === state.selectedRequest.metadata.name);
            if (!stillExists) {
                state.selectedRequest = null;
                state.auditRecords = [];
                renderDetails();
            } else {
                state.selectedRequest = stillExists;
                await fetchAuditRecords(stillExists.metadata.name);
            }
        }
    } catch (err) {
        console.error('Failed to fetch requests:', err);
    }
}

async function fetchAuditRecords(name) {
    // Clear immediately so the details pane doesn't show stale records from a
    // previously selected request while the new fetch is in flight.
    state.auditRecords = [];
    renderDetails();
    const req = state.requests.find(r => r.metadata.name === name);
    const ns = req?.metadata?.namespace || 'default';
    try {
        const response = await apiFetch(
            `/api/audit-records?agentRequest=${encodeURIComponent(name)}&namespace=${encodeURIComponent(ns)}`);
        if (!response.ok) return;
        state.auditRecords = await response.json();
        renderDetails();
    } catch (err) {
        console.error('Failed to fetch audit records:', err);
    }
}

async function performAction(name, action, reason) {
    const req = state.requests.find(r => r.metadata.name === name);
    const ns = req?.metadata?.namespace || 'default';
    try {
        const opts = { method: 'POST' };
        if (reason !== undefined) {
            opts.headers = { 'Content-Type': 'application/json' };
            opts.body = JSON.stringify({ reason });
        }
        const response = await apiFetch(
            `/api/agent-requests/${encodeURIComponent(name)}/${encodeURIComponent(action)}?namespace=${encodeURIComponent(ns)}`, opts);
        if (response.ok) {
            await fetchRequests();
        } else {
            const text = await response.text();
            alert('Action failed: ' + text);
        }
    } catch (err) {
        alert('Action failed: ' + err.message);
    }
}

// Called when the human clicks "Approve Override".
// If the control plane verified live endpoints, a reason is mandatory.
function promptApproval(name, hasActiveEndpoints) {
    if (!hasActiveEndpoints) {
        performAction(name, 'approve', '');
        return;
    }

    const overlay = document.createElement('div');
    overlay.id = 'reason-overlay';
    overlay.style.cssText = `
        position:fixed; inset:0; background:rgba(0,0,0,0.7); z-index:1000;
        display:flex; align-items:center; justify-content:center;`;

    overlay.innerHTML = `
        <div style="background:var(--surface-color);border:1px solid var(--border-color);border-radius:8px;padding:2rem;max-width:480px;width:90%;box-shadow:0 8px 32px rgba(0,0,0,0.5);">
            <h3 style="margin:0 0 0.5rem;color:var(--error);">&#9888; Override Required — Live Traffic Detected</h3>
            <p style="font-size:0.85rem;color:var(--text-secondary);margin:0 0 1.25rem;">
                The AIP control plane independently verified <strong style="color:var(--error);">active endpoints</strong>
                on this resource. You are approving a destructive action against live cluster evidence.
                <br><br>
                This justification will be recorded in the immutable audit trail.
            </p>
            <textarea id="override-reason" rows="4" placeholder="Why is this override safe? (required)"
                style="width:100%;box-sizing:border-box;background:rgba(255,255,255,0.05);border:1px solid var(--border-color);
                       border-radius:4px;padding:0.6rem;color:var(--text-primary);font-size:0.85rem;resize:vertical;"></textarea>
            <div style="display:flex;gap:0.75rem;margin-top:1rem;justify-content:flex-end;">
                <button onclick="document.getElementById('reason-overlay').remove()"
                    style="padding:0.5rem 1.25rem;background:transparent;border:1px solid var(--border-color);
                           border-radius:4px;color:var(--text-secondary);cursor:pointer;">Cancel</button>
                <button id="confirm-override-btn"
                    style="padding:0.5rem 1.25rem;background:var(--error);border:none;
                           border-radius:4px;color:white;font-weight:600;cursor:pointer;">Confirm Override</button>
            </div>
        </div>`;

    document.body.appendChild(overlay);
    document.getElementById('confirm-override-btn').addEventListener('click', () => {
        submitApprovalWithReason(name);
    });
    document.getElementById('override-reason').focus();
}

window.submitApprovalWithReason = async function(name) {
    const reason = document.getElementById('override-reason')?.value?.trim();
    if (!reason) {
        document.getElementById('override-reason').style.borderColor = 'var(--error)';
        return;
    }
    document.getElementById('reason-overlay')?.remove();
    await performAction(name, 'approve', reason);
};

function selectRequest(req) {
    state.selectedRequest = req;
    renderList();
    fetchAuditRecords(req.metadata.name);
}

function renderList() {
    const listEl = document.getElementById('request-list');
    if (state.requests.length === 0) {
        listEl.innerHTML = '<div class="empty-state">No requests found</div>';
        return;
    }

    listEl.innerHTML = state.requests.map(req => {
        const isActive = state.selectedRequest && state.selectedRequest.metadata.name === req.metadata.name;
        const phase = req.status?.phase || 'Pending';
        const time = new Date(req.metadata.creationTimestamp).toLocaleTimeString();

        return `
            <div class="request-item ${isActive ? 'active' : ''}" data-name="${escapeHtml(req.metadata.name)}" onclick="selectRequestById(this.dataset.name)">
                <div class="title">${escapeHtml(req.spec.agentIdentity)}</div>
                <div class="meta">
                    <span class="badge badge-${escapeHtml(phase.toLowerCase())}">${escapeHtml(phase)}</span>
                    <span>${time}</span>
                </div>
                <div style="font-size: 0.75rem; color: var(--text-secondary); margin-top: 0.4rem;">
                    ${escapeHtml(req.spec.action)} &rarr; ${escapeHtml(req.spec.target.uri)}
                </div>
            </div>
        `;
    }).join('');
}

window.selectRequestById = (name) => {
    const req = state.requests.find(r => r.metadata.name === name);
    if (req) selectRequest(req);
};

function conditionBadge(condition) {
    const positiveTypes = ['PolicyEvaluated', 'Approved', 'LockAcquired', 'Executing', 'Completed'];
    const isPositive = positiveTypes.includes(condition.type);
    const isTrue = condition.status === 'True';

    let color, label;
    if (condition.type === 'RequiresApproval' && isTrue) {
        color = 'var(--warning)'; label = 'PENDING REVIEW';
    } else if (isPositive && isTrue) {
        color = 'var(--success)'; label = 'TRUE';
    } else if (!isPositive && isTrue) {
        color = 'var(--error)'; label = 'TRUE';
    } else {
        color = 'var(--text-secondary)'; label = 'FALSE';
    }
    return `<span style="color: ${color}; font-weight: 600; font-size: 0.8rem;">${label}</span>`;
}

function renderReasoningTrace(rt) {
    const confidence = rt.confidenceScore ?? null;
    const pct = confidence !== null ? Math.round(confidence * 100) : null;
    const color = pct === null ? 'var(--text-secondary)' : pct >= 80 ? 'var(--success)' : pct >= 60 ? 'var(--warning)' : 'var(--error)';

    const componentRows = rt.componentConfidence
        ? Object.entries(rt.componentConfidence).map(([k, v]) => {
            const p = Math.round(parseFloat(v) * 100);
            const c = p >= 80 ? 'var(--success)' : p >= 60 ? 'var(--warning)' : 'var(--error)';
            return `<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:0.4rem;font-size:0.82rem;">
                <span style="color:var(--text-secondary);">${escapeHtml(k.replace(/_/g,' '))}</span>
                <span style="color:${c};font-weight:600;">${p}%</span>
            </div>`;
        }).join('') : '';

    const alternatives = rt.alternatives?.length
        ? rt.alternatives.map(a => `<span style="display:inline-block;padding:0.2rem 0.6rem;background:rgba(255,255,255,0.05);border-radius:9999px;font-size:0.78rem;margin:0.2rem 0.2rem 0 0;color:var(--text-secondary);">${escapeHtml(a)}</span>`).join('')
        : '<span style="color:var(--text-secondary);font-size:0.82rem;">None declared</span>';

    const traceLink = rt.traceReference
        ? `<div style="margin-top:0.75rem;font-size:0.78rem;"><span style="color:var(--text-secondary);">Trace: </span><span style="font-family:monospace;color:var(--accent-color);">${escapeHtml(rt.traceReference)}</span></div>`
        : '';

    return `
        <div class="panel" style="margin-top:1.5rem;">
            <div class="panel-title">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 19c-5 1.5-5-2.5-7-3m14 6v-3.87a3.37 3.37 0 0 0-.94-2.61c3.14-.35 6.44-1.54 6.44-7A5.44 5.44 0 0 0 20 4.77 5.07 5.07 0 0 0 19.91 1S18.73.65 16 2.48a13.38 13.38 0 0 0-7 0C6.27.65 5.09 1 5.09 1A5.07 5.07 0 0 0 5 4.77a5.44 5.44 0 0 0-1.5 3.78c0 5.42 3.3 6.61 6.44 7A3.37 3.37 0 0 0 9 18.13V22"></path></svg>
                Agent Reasoning Trace
            </div>
            ${pct !== null ? `
                <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem;">
                    <span style="font-size:0.85rem;color:var(--text-secondary);">Overall confidence</span>
                    <span style="font-size:1.4rem;font-weight:700;color:${color};">${pct}%</span>
                    <div class="confidence-meter" style="flex:1;"><div class="confidence-fill" style="width:${pct}%;background:${color};"></div></div>
                </div>
                ${componentRows ? `<div style="margin-bottom:1rem;">${componentRows}</div>` : ''}
            ` : ''}
            <div style="font-size:0.82rem;color:var(--text-secondary);margin-bottom:0.4rem;text-transform:uppercase;letter-spacing:0.05em;">Alternatives considered</div>
            <div style="margin-bottom:0.5rem;">${alternatives}</div>
            ${traceLink}
        </div>`;
}

function renderParameters(params) {
    if (!params || Object.keys(params).length === 0) return '';
    const rows = Object.entries(params).map(([k, v]) => {
        const color = v === false ? 'var(--error)' : 'var(--text-primary)';
        return `<div style="display:flex;justify-content:space-between;padding:0.4rem 0;border-bottom:1px solid var(--border-color);font-size:0.85rem;">
            <span style="color:var(--text-secondary);">${escapeHtml(k)}</span>
            <span style="font-family:monospace;color:${color};">${escapeHtml(JSON.stringify(v))}</span>
        </div>`;
    }).join('');
    return `
        <div class="panel" style="margin-top:1.5rem;">
            <div class="panel-title">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="16 18 22 12 16 6"></polyline><polyline points="8 6 2 12 8 18"></polyline></svg>
                Deployment Parameters
            </div>
            ${rows}
        </div>`;
}

function renderControlPlaneVerification(cpv) {
    if (!cpv) {
        return `<div style="color:var(--text-secondary);font-size:0.82rem;padding:0.5rem 0;">No verification data yet — pending evaluation.</div>`;
    }

    const check = `<span style="color:var(--error);font-weight:700;">&#x2717;</span>`;
    const ok    = `<span style="color:var(--success);font-weight:700;">&#x2713;</span>`;

    const rows = [
        { icon: cpv.targetExists ? ok : check,          label: 'Target exists',    value: cpv.targetExists ? 'yes' : 'not found' },
        { icon: cpv.hasActiveEndpoints ? check : ok,     label: 'Active endpoints', value: cpv.hasActiveEndpoints ? `${cpv.activeEndpointCount} detected` : 'none' },
        { icon: cpv.readyReplicas > 1 ? check : ok,      label: 'Ready replicas',   value: `${cpv.readyReplicas} / ${cpv.specReplicas} spec` },
    ];

    const rowsHtml = rows.map(r => `
        <div style="display:flex;align-items:center;gap:0.6rem;padding:0.35rem 0;font-size:0.83rem;border-bottom:1px solid var(--border-color);">
            ${r.icon}
            <span style="color:var(--text-secondary);flex:1;">${r.label}</span>
            <span style="font-family:'JetBrains Mono',monospace;">${r.value}</span>
        </div>`).join('');

    const downstream = cpv.downstreamServices?.length
        ? cpv.downstreamServices.map(s => `
            <div style="display:flex;align-items:center;gap:0.5rem;padding:0.3rem 0;font-size:0.8rem;">
                ${check}
                <span style="font-family:'JetBrains Mono',monospace;color:#cbd5e1;">${escapeHtml(s)}</span>
                <span style="color:var(--text-secondary);font-size:0.72rem;">(would be disrupted)</span>
            </div>`).join('')
        : `<div style="color:var(--text-secondary);font-size:0.8rem;">None detected</div>`;

    const fetched = cpv.fetchedAt ? new Date(cpv.fetchedAt).toLocaleTimeString() : '';

    return `
        <div>${rowsHtml}</div>
        <div style="margin-top:0.75rem;">
            <div style="font-size:0.78rem;text-transform:uppercase;letter-spacing:0.05em;color:var(--text-secondary);margin-bottom:0.4rem;">Downstream (Control Plane Detected)</div>
            ${downstream}
        </div>
        ${fetched ? `<div style="margin-top:0.6rem;font-size:0.7rem;color:var(--text-secondary);">Verified at ${fetched}</div>` : ''}`;
}

// G-8: render providerContext — free-form JSON from context fetchers
function renderProviderContext(ctx) {
    if (!ctx) return '';
    let parsed;
    try {
        parsed = typeof ctx === 'string' ? JSON.parse(ctx) : ctx;
    } catch (_) {
        parsed = ctx;
    }

    const rows = Object.entries(parsed).map(([k, v]) => {
        const display = typeof v === 'object' ? JSON.stringify(v) : String(v);
        return `<div style="display:flex;justify-content:space-between;padding:0.35rem 0;border-bottom:1px solid var(--border-color);font-size:0.83rem;">
            <span style="color:var(--text-secondary);">${escapeHtml(k)}</span>
            <span style="font-family:'JetBrains Mono',monospace;">${escapeHtml(display)}</span>
        </div>`;
    }).join('');

    return `
        <div style="margin-top:1rem;">
            <div style="font-size:0.78rem;text-transform:uppercase;letter-spacing:0.05em;color:var(--text-secondary);margin-bottom:0.5rem;">Provider Context</div>
            ${rows}
        </div>`;
}

function renderGovernanceTimeline(phase, needsApproval) {
    const isSoak = phase === 'AwaitingVerdict' || (phase === 'Completed' && state.selectedRequest?.status?.verdict);
    
    const steps = isSoak ? [
        { label: 'Intent declared',  done: true },
        { label: 'Policy evaluated', done: true },
        { label: 'Human grade',      done: phase === 'Completed', active: phase === 'AwaitingVerdict' },
    ] : [
        { label: 'Intent declared',  done: true },
        { label: 'Policy evaluated', done: true },
        { label: 'Human gate',       done: ['Approved','Denied','Executing','Completed'].includes(phase), active: needsApproval, denied: phase === 'Denied' },
        { label: 'Action executed',  done: phase === 'Completed', active: phase === 'Executing' },
    ];

    const stepHtml = steps.map((s, i) => {
        let dotColor, dotContent, labelColor;
        if (s.denied)       { dotColor = 'var(--error)';       dotContent = '&#x2715;'; labelColor = 'var(--error)'; }
        else if (s.active)  { dotColor = 'var(--warning)';     dotContent = '&#x25C9;'; labelColor = 'var(--warning)'; }
        else if (s.done)    { dotColor = 'var(--success)';     dotContent = '&#x25CF;'; labelColor = 'var(--text-secondary)'; }
        else                { dotColor = 'var(--border-color)'; dotContent = '&#x25CB;'; labelColor = 'var(--border-color)'; }

        const connector = i < steps.length - 1
            ? `<div style="flex:1;height:1px;background:${s.done ? 'var(--success)' : 'var(--border-color)'};margin:0 0.5rem;opacity:0.5;"></div>`
            : '';

        return `
            <div style="display:flex;align-items:center;">
                <div style="display:flex;flex-direction:column;align-items:center;gap:0.3rem;">
                    <span style="color:${dotColor};font-size:0.85rem;line-height:1;">${dotContent}</span>
                    <span style="font-size:0.68rem;color:${labelColor};white-space:nowrap;font-weight:${s.active ? '700' : '400'};">${s.label}</span>
                </div>
                ${connector}
            </div>`;
    }).join('');

    return `
        <div style="display:flex;align-items:flex-start;padding:0.75rem 1rem;background:rgba(255,255,255,0.02);border:1px solid var(--border-color);border-radius:6px;margin-top:1rem;">
            ${stepHtml}
        </div>`;
}

function renderDetails() {
    const detailsEl = document.getElementById('details-view');
    const req = state.selectedRequest;
    if (!req) {
        detailsEl.innerHTML = `
            <div class="empty-state">
                <svg width="64" height="64" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M14.5 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7.5L14.5 2z"></path>
                    <polyline points="14 2 14 8 20 8"></polyline>
                    <line x1="16" y1="13" x2="8" y2="13"></line>
                    <line x1="16" y1="17" x2="8" y2="17"></line>
                    <line x1="10" y1="9" x2="8" y2="9"></line>
                </svg>
                <p>Select an AgentRequest to view details</p>
            </div>`;
        return;
    }

    const phase = req.status?.phase || 'Pending';
    const auditLogs = [...state.auditRecords].sort((a, b) => new Date(a.spec.timestamp) - new Date(b.spec.timestamp));
    const needsApproval = phase === 'Pending' && req.status?.conditions?.some(c => c.type === 'RequiresApproval' && c.status === 'True');
    const isReviewer = state.role === 'reviewer' || state.role === 'admin';
    const reason = req.spec.reason || 'No reason provided.';
    const name = req.metadata.name;

    let verdictInfo = '';
    if (req.status?.verdict) {
        const v = req.status.verdict;
        let vclass = 'badge-executing';
        if (v === 'correct') vclass = 'badge-completed';
        else if (v === 'incorrect') vclass = 'badge-failed';
        else if (v === 'partial') vclass = 'badge-warning';

        verdictInfo = `
            <div style="margin-top:1.25rem;padding:1rem;background:rgba(255,255,255,0.02);border:1px solid var(--border-color);border-radius:8px;">
                <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:0.75rem;">
                    <span style="font-size:0.78rem;text-transform:uppercase;letter-spacing:0.05em;color:var(--text-secondary);">Human Verdict</span>
                    <span class="badge ${vclass}">${escapeHtml(v)}</span>
                </div>
                ${req.status.verdictReasonCode ? `<div style="font-size:0.85rem;margin-bottom:0.5rem;"><span style="color:var(--text-secondary);">Reason:</span> ${escapeHtml(req.status.verdictReasonCode)}</div>` : ''}
                ${req.status.verdictNote ? `<div style="font-size:0.85rem;margin-bottom:0.5rem;font-style:italic;">&ldquo;${escapeHtml(req.status.verdictNote)}&rdquo;</div>` : ''}
                <div style="font-size:0.72rem;color:var(--text-secondary);text-align:right;">Graded by ${escapeHtml(req.status.verdictBy || 'unknown')} at ${req.status.verdictAt ? new Date(req.status.verdictAt).toLocaleString() : ''}</div>
            </div>`;
    }

    // G-10: GOVERNED_RESOURCE_DELETED banner
    const grDeletedBanner = req.status?.denial?.code === 'GOVERNED_RESOURCE_DELETED'
        ? `<div style="margin-top:0.75rem;padding:0.6rem 0.85rem;background:rgba(239,68,68,0.1);border:1px solid var(--error);border-radius:6px;font-size:0.83rem;color:var(--error);">
            &#x26A0; This request was denied because the GovernedResource that admitted it was deleted after submission.
           </div>`
        : '';

    // G-9: FetcherSchemaViolation banner
    const fetcherViolation = req.status?.conditions?.find(c => c.type === 'FetcherSchemaViolation' && c.status === 'True');
    const fetcherBanner = fetcherViolation
        ? `<div style="margin-top:0.75rem;padding:0.6rem 0.85rem;background:rgba(245,158,11,0.1);border:1px solid var(--warning);border-radius:6px;font-size:0.83rem;color:var(--warning);">
            &#x26A0; Context fetch failed — reviewer is operating without live resource data.
            <div style="margin-top:0.25rem;color:var(--text-secondary);font-size:0.78rem;">${escapeHtml(fetcherViolation.message || '')}</div>
           </div>`
        : '';

    // G-7: governedResourceRef
    const grRef = req.spec.governedResourceRef;
    const grRefRow = grRef
        ? `<div style="display:flex;justify-content:space-between;padding:0.35rem 0;border-bottom:1px solid var(--border-color);font-size:0.83rem;">
            <span style="color:var(--text-secondary);">Governed by</span>
            <span style="font-family:'JetBrains Mono',monospace;">${escapeHtml(grRef.name)} (generation ${escapeHtml(String(grRef.generation ?? ''))})</span>
           </div>`
        : `<div style="display:flex;justify-content:space-between;padding:0.35rem 0;border-bottom:1px solid var(--border-color);font-size:0.83rem;">
            <span style="color:var(--text-secondary);">Governed by</span>
            <span style="color:var(--text-secondary);font-style:italic;">none (no GovernedResource matched)</span>
           </div>`;

    detailsEl.innerHTML = `
        <div style="display:flex;justify-content:space-between;align-items:flex-start;">
            <div>
                <h2 style="margin-bottom:0.5rem;">${escapeHtml(req.spec.agentIdentity)}</h2>
                <div style="color:var(--text-secondary);font-size:0.9rem;">
                    ${escapeHtml(name)} &nbsp;&middot;&nbsp; ${escapeHtml(req.spec.action)} on <code style="font-size:0.85rem;">${escapeHtml(req.spec.target.uri)}</code>
                </div>
            </div>
            <span class="badge badge-${escapeHtml(phase.toLowerCase())}" style="font-size:1rem;padding:0.5rem 1rem;">${escapeHtml(phase)}</span>
        </div>

        ${grDeletedBanner}
        ${fetcherBanner}
        ${renderGovernanceTimeline(phase, needsApproval)}

        <div class="side-by-side" style="margin-top:1rem;">
            <div class="panel">
                <div class="panel-title">
                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"></path></svg>
                    Agent Declared
                </div>
                ${grRefRow}
                <div class="reasoning-content" style="margin:0.75rem 0 1rem;">${escapeHtml(reason)}</div>
                <div style="font-size:0.78rem;text-transform:uppercase;letter-spacing:0.05em;color:var(--text-secondary);margin-bottom:0.5rem;">Confidence</div>
                ${req.spec.reasoningTrace?.confidenceScore != null ? (() => {
                    const pct = Math.round(req.spec.reasoningTrace.confidenceScore * 100);
                    const color = pct >= 80 ? 'var(--success)' : pct >= 60 ? 'var(--warning)' : 'var(--error)';
                    return `<div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:1rem;">
                        <span style="font-size:1.3rem;font-weight:700;color:${color};">${pct}%</span>
                        <div class="confidence-meter" style="flex:1;"><div class="confidence-fill" style="width:${pct}%;background:${color};"></div></div>
                    </div>`;
                })() : '<div style="color:var(--text-secondary);font-size:0.82rem;margin-bottom:1rem;">Not declared</div>'}
                <div style="font-size:0.78rem;text-transform:uppercase;letter-spacing:0.05em;color:var(--text-secondary);margin-bottom:0.5rem;">Downstream (Causal Model)</div>
                ${req.spec.cascadeModel?.affectedTargets?.length
                    ? req.spec.cascadeModel.affectedTargets.map(t => {
                        const effectColors = { disrupted:'var(--error)', modified:'var(--warning)', deleted:'#ff4444', orphaned:'var(--text-secondary)' };
                        const color = effectColors[t.effectType] || 'var(--text-secondary)';
                        return `<div style="display:flex;align-items:center;gap:0.5rem;padding:0.3rem 0;font-size:0.8rem;">
                            <span style="color:${color};font-weight:700;min-width:65px;font-size:0.7rem;text-transform:uppercase;">${escapeHtml(t.effectType)}</span>
                            <span style="font-family:'JetBrains Mono',monospace;color:#cbd5e1;">${escapeHtml(t.uri.split('/').pop())}</span>
                        </div>`;
                    }).join('')
                    : '<div style="color:var(--text-secondary);font-size:0.82rem;">None declared</div>'}
                ${req.spec.cascadeModel?.modelSourceTrust
                    ? `<div style="margin-top:0.75rem;font-size:0.75rem;color:var(--text-secondary);">Model source trust: <strong style="color:var(--text-primary);">${escapeHtml(req.spec.cascadeModel.modelSourceTrust)}</strong></div>`
                    : ''}
            </div>

            <div class="panel">
                <div class="panel-title" style="color:var(--warning);">
                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"></path></svg>
                    Control Plane Verified
                </div>
                ${renderControlPlaneVerification(req.status?.controlPlaneVerification)}
                ${renderProviderContext(req.status?.providerContext)}
                ${verdictInfo}
                <div style="margin-top:1.25rem;">
                    <div style="font-size:0.78rem;text-transform:uppercase;letter-spacing:0.05em;color:var(--text-secondary);margin-bottom:0.5rem;">Policy Conditions</div>
                    ${req.status?.conditions?.filter(c => c.type !== 'FetcherSchemaViolation').map(c => `
                        <div style="margin-bottom:0.6rem;padding:0.4rem 0.5rem;background:rgba(255,255,255,0.03);border-radius:4px;">
                            <div style="display:flex;justify-content:space-between;font-size:0.82rem;">
                                <span style="font-weight:600;">${escapeHtml(c.type)}</span>
                                ${conditionBadge(c)}
                            </div>
                            <div style="font-size:0.72rem;color:var(--text-secondary);margin-top:0.15rem;">${escapeHtml(c.message || '')}</div>
                        </div>
                    `).join('') || '<div style="color:var(--text-secondary);font-size:0.85rem;">No conditions yet</div>'}
                </div>
                ${(needsApproval || phase === 'AwaitingVerdict') && isReviewer ? `
                    <div class="actions">
                        ${phase === 'AwaitingVerdict' ? 
                            `<button class="primary" onclick="gradeAgentRequest('${escapeHtml(name)}')">Grade Diagnosis</button>` :
                            `<button class="primary" data-name="${escapeHtml(name)}" data-endpoints="${!!req.status?.controlPlaneVerification?.hasActiveEndpoints}" onclick="promptApproval(this.dataset.name, this.dataset.endpoints === 'true')">Approve Override</button>
                             <button class="danger" data-name="${escapeHtml(name)}" onclick="confirmDenial(this.dataset.name)">Deny</button>`
                        }
                    </div>
                ` : needsApproval || phase === 'AwaitingVerdict' ? `
                    <div style="margin-top:1rem;font-size:0.82rem;color:var(--text-secondary);font-style:italic;">Awaiting reviewer ${phase === 'AwaitingVerdict' ? 'grade' : 'approval'}.</div>
                ` : ''}
            </div>
        </div>

        ${req.spec.reasoningTrace?.alternatives?.length || req.spec.reasoningTrace?.componentConfidence ? renderReasoningTrace(req.spec.reasoningTrace) : ''}
        ${req.spec.parameters ? renderParameters(req.spec.parameters) : ''}

        <div class="audit-logs" style="margin-top:1.5rem;">
            <h3 style="margin-bottom:1rem;">Audit Trail</h3>
            ${auditLogs.length === 0
                ? '<div style="color:var(--text-secondary);font-size:0.85rem;">No audit records yet</div>'
                : auditLogs.map(log => {
                    // G-11: policyGeneration in denial events
                    const policyResults = log.spec.policyResults?.length
                        ? `<div style="margin-top:0.3rem;font-size:0.72rem;color:var(--text-secondary);">` +
                          log.spec.policyResults.map(pr =>
                            `<span style="margin-right:0.75rem;">${escapeHtml(pr.policyName)}/${escapeHtml(pr.ruleName)}: ${escapeHtml(pr.result)}${pr.policyGeneration != null ? ' (gen ' + escapeHtml(String(pr.policyGeneration)) + ')' : ''}</span>`
                          ).join('') + `</div>`
                        : '';
                    return `
                    <div class="audit-item">
                        <span class="time">${new Date(log.spec.timestamp).toLocaleTimeString()}</span>
                        <span style="font-weight:600;color:var(--accent-color);">${escapeHtml(log.spec.event)}</span>
                        ${log.spec.phaseTransition ? `
                            <span style="color:var(--text-secondary);">&nbsp;(${escapeHtml(log.spec.phaseTransition.from)} &rarr; ${escapeHtml(log.spec.phaseTransition.to)})</span>
                        ` : ''}
                        ${policyResults}
                    </div>`;
                }).join('')}
        </div>`;
}

// ── Diagnostics tab ──────────────────────────────────────────────────────────

window.showTab = function(tabName) {
    const views = ['requests', 'diagnostics', 'governed-resources', 'safety-policies'];
    for (const v of views) {
        const el = document.getElementById(v + '-view');
        if (el) el.style.display = v === tabName ? 'block' : 'none';
        const tab = document.getElementById('tab-' + v);
        if (tab) tab.classList.toggle('active', v === tabName);
    }
    if (tabName === 'diagnostics') loadDiagnostics();
    if (tabName === 'governed-resources') loadGovernedResources();
    if (tabName === 'safety-policies') loadSafetyPolicies();
};

window.loadDiagnostics = async function() {
    if (!getToken() && !state.proxyAuth) {
        state.diagnosticsGen++; // Invalidate any in-flight requests
        state.diagnostics = [];
        state.diagnosticsJSON = '';
        renderDiagnostics();
        const container = document.getElementById('accuracy-chip-container');
        if (container) {
            container.style.display = 'none';
            container.innerHTML = '';
        }
        return;
    }
    try {
        const ns = document.getElementById('ns-input')?.value.trim() || state.namespace;
        state.namespace = ns;
        const gen = ++state.diagnosticsGen;
        const response = await apiFetch(`/api/agent-diagnostics?namespace=${encodeURIComponent(ns)}`);
        if (!response.ok) throw new Error('Failed to fetch diagnostics');
        const fresh = await response.json();
        if (gen !== state.diagnosticsGen) return;
        const freshJSON = ns + ':' + JSON.stringify(fresh);
        if (freshJSON !== state.diagnosticsJSON) {
            state.diagnosticsJSON = freshJSON;
            state.diagnostics = fresh;
        }
        await fetchSummaries(gen, ns);
        if (gen === state.diagnosticsGen) renderDiagnostics();
    } catch (err) {
        console.error('Error fetching diagnostics:', err);
    }
};

window.fetchSummaries = async function(gen, ns) {
    const container = document.getElementById('accuracy-chip-container');
    try {
        const response = await apiFetch(`/api/diagnostic-accuracy-summaries?namespace=${encodeURIComponent(ns)}`);
        if (gen !== state.diagnosticsGen) return;
        if (!response.ok) {
            if (container) { container.style.display = 'none'; container.innerHTML = ''; }
            return;
        }
        const summaries = await response.json();
        if (!container) return;

        if (!summaries || summaries.length === 0) {
            container.style.display = 'none';
            return;
        }

        container.style.display = 'block';
        const rows = summaries.map(s => {
            const acc = s.status?.diagnosticAccuracy;
            if (acc == null) return '';
            const pct = Math.round(acc * 100);
            const color = pct >= 80 ? 'var(--success)' : pct >= 60 ? 'var(--warning)' : 'var(--error)';
            const total = s.status?.totalReviewed ?? 0;
            const correct = s.status?.correctCount ?? 0;
            const partial = s.status?.partialCount ?? 0;
            const incorrect = s.status?.incorrectCount ?? 0;
            const barWidth = pct;
            return `<tr>
                <td style="font-family:'JetBrains Mono',monospace;font-size:0.82rem;white-space:nowrap;padding:0.35rem 0.75rem 0.35rem 0;">${escapeHtml(s.spec.agentIdentity)}</td>
                <td style="padding:0.35rem 0.75rem;width:140px;">
                    <div style="position:relative;height:6px;background:rgba(255,255,255,0.08);border-radius:3px;overflow:hidden;">
                        <div style="position:absolute;left:0;top:0;height:100%;width:${barWidth}%;background:${color};border-radius:3px;"></div>
                    </div>
                </td>
                <td style="padding:0.35rem 0.5rem;font-size:0.82rem;font-weight:600;color:${color};white-space:nowrap;">${pct}%</td>
                <td style="padding:0.35rem 0.75rem;font-size:0.78rem;color:var(--text-secondary);white-space:nowrap;">${total} reviewed</td>
                <td style="padding:0.35rem 0.75rem;font-size:0.78rem;color:var(--text-secondary);white-space:nowrap;">✓ ${correct} &nbsp; ~ ${partial} &nbsp; ✗ ${incorrect}</td>
            </tr>`;
        }).join('');
        container.innerHTML = `<table style="border-collapse:collapse;font-size:0.82rem;margin-bottom:0.5rem;">${rows}</table>`;
    } catch (e) {
        console.error('Failed to fetch summaries', e);
        if (container) { container.style.display = 'none'; container.innerHTML = ''; }
    }
};

async function fetchRequestSummaries(gen, ns) {
    const container = document.getElementById('requests-accuracy-chip-container');
    try {
        const response = await apiFetch(`/api/diagnostic-accuracy-summaries?namespace=${encodeURIComponent(ns)}`);
        if (gen !== state.requestsGen) return;
        if (!response.ok) {
            if (container) { container.style.display = 'none'; container.innerHTML = ''; }
            return;
        }
        const summaries = await response.json();
        if (!container) return;

        if (!summaries || summaries.length === 0) {
            container.style.display = 'none';
            return;
        }

        container.style.display = 'block';
        const rows = summaries.map(s => {
            const acc = s.status?.diagnosticAccuracy;
            if (acc == null) return '';
            const pct = Math.round(acc * 100);
            const color = pct >= 80 ? 'var(--success)' : pct >= 60 ? 'var(--warning)' : 'var(--error)';
            const total = s.status?.totalReviewed ?? 0;
            const correct = s.status?.correctCount ?? 0;
            const partial = s.status?.partialCount ?? 0;
            const incorrect = s.status?.incorrectCount ?? 0;
            const barWidth = pct;
            return `<tr>
                <td style="font-family:'JetBrains Mono',monospace;font-size:0.82rem;white-space:nowrap;padding:0.35rem 0.75rem 0.35rem 0;">${escapeHtml(s.spec.agentIdentity)}</td>
                <td style="padding:0.35rem 0.75rem;width:140px;">
                    <div style="position:relative;height:6px;background:rgba(255,255,255,0.08);border-radius:3px;overflow:hidden;">
                        <div style="position:absolute;left:0;top:0;height:100%;width:${barWidth}%;background:${color};border-radius:3px;"></div>
                    </div>
                </td>
                <td style="padding:0.35rem 0.5rem;font-size:0.82rem;font-weight:600;color:${color};white-space:nowrap;">${pct}%</td>
                <td style="padding:0.35rem 0.75rem;font-size:0.78rem;color:var(--text-secondary);white-space:nowrap;">${total} reviewed</td>
                <td style="padding:0.35rem 0.75rem;font-size:0.78rem;color:var(--text-secondary);white-space:nowrap;">✓ ${correct} &nbsp; ~ ${partial} &nbsp; ✗ ${incorrect}</td>
            </tr>`;
        }).join('');
        container.innerHTML = `<table style="border-collapse:collapse;font-size:0.82rem;margin-bottom:0.5rem;">${rows}</table>`;
    } catch (e) {
        console.error('Failed to fetch summaries', e);
        if (container) { container.style.display = 'none'; container.innerHTML = ''; }
    }
}

function renderDiagnostics() {
    const listEl = document.getElementById('diagnostics-list');
    if (!listEl) return;

    const showVerdict = state.role === 'reviewer' || state.role === 'admin';
    const colCount = showVerdict ? 6 : 5;
    const verdictHeader = document.getElementById('th-diag-verdict');
    if (verdictHeader) verdictHeader.style.display = showVerdict ? '' : 'none';

    if (!state.diagnostics || state.diagnostics.length === 0) {
        listEl.innerHTML = `
            <tr><td colspan="${colCount}" style="text-align:center;padding:3rem;color:var(--text-secondary);">
                No diagnostics found in namespace &ldquo;${escapeHtml(state.namespace)}&rdquo;
            </td></tr>`;
        return;
    }

    const sorted = [...state.diagnostics].sort((a, b) =>
        new Date(b.metadata.creationTimestamp) - new Date(a.metadata.creationTimestamp)
    );

    listEl.innerHTML = sorted.map(diag => {
        const age = formatAge(diag.metadata.creationTimestamp);
        const name = diag.metadata.name;
        const verdict = diag.status?.verdict;

        // Merge correlationID into the details object for display
        const detailsObj = { ...(diag.spec.details || {}) };
        if (diag.spec.correlationID) detailsObj.correlationID = diag.spec.correlationID;
        const hasDetails = Object.keys(detailsObj).length > 0;

        let verdictCell = '';
        if (showVerdict) {
            let verdictBadge = '';
            let gradeLabel = 'Grade';
            if (verdict) {
                let vclass = 'badge-executing';
                if (verdict === 'correct') vclass = 'badge-completed';
                else if (verdict === 'incorrect') vclass = 'badge-failed';
                else if (verdict === 'partial') vclass = 'badge-warning';
                verdictBadge = `<span class="badge ${vclass}" style="margin-right:0.4rem;">${escapeHtml(verdict)}</span>`;
                gradeLabel = 'Edit';
            }

            verdictCell = `<td class="action-cell">
                <div style="display:flex;align-items:center;gap:0.4rem;">
                    ${verdictBadge}
                    <button class="details-btn" onclick="gradeDiagnostic('${escapeHtml(name)}')">${gradeLabel}</button>
                </div>
            </td>`;
        }

        // Summary column gets summary-cell class for better wrapping
        return `
            <tr>
                <td style="white-space:nowrap;color:var(--text-secondary);">${age}</td>
                <td><span class="chip">${escapeHtml(diag.spec.agentIdentity)}</span></td>
                <td><span class="badge ${getDiagnosticTypeClass(diag.spec.diagnosticType)}">${escapeHtml(diag.spec.diagnosticType)}</span></td>
                <td class="summary-cell">${escapeHtml(diag.spec.summary)}</td>
                <td class="action-cell">${hasDetails
                    ? `<button class="details-btn" onclick="viewDiagnosticDetails('${escapeHtml(name)}')">View JSON</button>`
                    : '<span style="color:var(--text-secondary);font-size:0.8rem;">None</span>'
                }</td>
                ${verdictCell}
            </tr>`;
    }).join('');
}

function getDiagnosticTypeClass(type) {
    const t = (type || '').toLowerCase();
    if (t.includes('error') || t.includes('fail')) return 'badge-failed';
    if (t.includes('warn')) return 'badge-denied';
    if (t.includes('success') || t.includes('observation') || t.includes('diagnosis')) return 'badge-completed';
    return 'badge-executing';
}

function formatAge(timestamp) {
    const diff = Math.floor((Date.now() - new Date(timestamp)) / 1000);
    if (diff < 60) return diff + 's';
    if (diff < 3600) return Math.floor(diff / 60) + 'm';
    if (diff < 86400) return Math.floor(diff / 3600) + 'h';
    return Math.floor(diff / 86400) + 'd';
}

// ── Governed Resources tab (G-5, admin only) ─────────────────────────────────

async function loadGovernedResources() {
    const tbody = document.getElementById('gr-list');
    try {
        const resp = await apiFetch('/api/governed-resources');
        if (!resp.ok) {
            tbody.innerHTML = `<tr><td colspan="6" style="text-align:center;padding:2rem;color:var(--error);">Failed to load (HTTP ${resp.status})</td></tr>`;
            return;
        }
        const items = (await resp.json()).items || [];
        if (items.length === 0) {
            tbody.innerHTML = `<tr><td colspan="6" style="text-align:center;padding:2rem;color:var(--text-secondary);">No governed resources found.</td></tr>`;
            return;
        }
        tbody.innerHTML = items.map(gr => `
            <tr>
                <td style="font-family:'JetBrains Mono',monospace;">${escapeHtml(gr.metadata?.name || gr.name || '')}</td>
                <td style="font-family:'JetBrains Mono',monospace;font-size:0.8rem;">${escapeHtml(gr.spec?.uriPattern || gr.uriPattern || '')}</td>
                <td><span class="chip">${escapeHtml(gr.spec?.contextFetcher || gr.contextFetcher || 'none')}</span></td>
                <td style="font-size:0.8rem;">${escapeHtml((gr.spec?.permittedActions || gr.permittedActions || []).join(', '))}</td>
                <td style="font-size:0.8rem;">${escapeHtml((gr.spec?.permittedAgents || gr.permittedAgents || []).join(', ') || '(any)')}</td>
                <td>
                    <button class="details-btn" data-name="${escapeHtml(gr.metadata?.name || gr.name || '')}" onclick="openGRForm(this.dataset.name)" style="margin-right:0.4rem;">Edit</button>
                    <button class="details-btn" data-name="${escapeHtml(gr.metadata?.name || gr.name || '')}" onclick="deleteGR(this.dataset.name)" style="color:var(--error);">Delete</button>
                </td>
            </tr>`).join('');
    } catch (e) {
        tbody.innerHTML = `<tr><td colspan="6" style="text-align:center;padding:2rem;color:var(--error);">Error: ${escapeHtml(e.message)}</td></tr>`;
    }
}

window.openGRForm = async function(name) {
    const container = document.getElementById('gr-form-container');
    container.style.display = 'block';

    let existing = null;
    if (name) {
        const resp = await apiFetch(`/api/governed-resources/${encodeURIComponent(name)}`);
        if (resp.ok) existing = await resp.json();
    }
    const spec = existing?.spec || existing || {};

    container.innerHTML = `
        <div style="max-width:600px;">
            <h4 style="margin:0 0 0.75rem;">${name ? 'Edit' : 'Create'} Governed Resource</h4>
            <div style="display:grid;grid-template-columns:1fr 1fr;gap:0.5rem;margin-bottom:0.5rem;">
                <label style="font-size:0.82rem;color:var(--text-secondary);">Name *
                    <input id="gr-name" type="text" value="${escapeHtml(name || '')}" ${name ? 'readonly' : ''}
                        style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;${name ? 'opacity:0.6;' : ''}"/>
                </label>
                <label style="font-size:0.82rem;color:var(--text-secondary);">Context Fetcher
                    <select id="gr-fetcher" style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;">
                        ${['none','karpenter','github','k8s-deployment'].map(f => `<option value="${f}"${(spec.contextFetcher||'none')===f?' selected':''}>${f}</option>`).join('')}
                    </select>
                </label>
            </div>
            <label style="font-size:0.82rem;color:var(--text-secondary);display:block;margin-bottom:0.5rem;">URI Pattern *
                <input id="gr-pattern" type="text" value="${escapeHtml(spec.uriPattern || '')}" placeholder="k8s://prod/karpenter.sh/nodepool/*"
                    style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;font-family:'JetBrains Mono',monospace;"/>
            </label>
            <div style="display:grid;grid-template-columns:1fr 1fr;gap:0.5rem;margin-bottom:0.5rem;">
                <label style="font-size:0.82rem;color:var(--text-secondary);">Permitted Actions * (comma-separated)
                    <input id="gr-actions" type="text" value="${escapeHtml((spec.permittedActions||[]).join(', '))}" placeholder="scale-up, scale-down"
                        style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;"/>
                </label>
                <label style="font-size:0.82rem;color:var(--text-secondary);">Permitted Agents (comma-separated, empty = any)
                    <input id="gr-agents" type="text" value="${escapeHtml((spec.permittedAgents||[]).join(', '))}" placeholder="aip-agent-1"
                        style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;"/>
                </label>
            </div>
            <label style="font-size:0.82rem;color:var(--text-secondary);display:block;margin-bottom:0.75rem;">Description
                <input id="gr-desc" type="text" value="${escapeHtml(spec.description || '')}"
                    style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;"/>
            </label>
            <div style="margin-bottom:0.75rem;">
                <label style="font-size:0.82rem;color:var(--text-secondary);display:flex;align-items:center;gap:0.5rem;cursor:pointer;">
                    <input id="gr-soak" type="checkbox" ${spec.soakMode ? 'checked' : ''} style="width:auto;margin:0;"/>
                    Soak Mode (routes requests to AwaitingVerdict phase)
                </label>
            </div>
            <div style="display:flex;gap:0.5rem;">
                <button data-name="${escapeHtml(name || '')}" onclick="submitGRForm(this.dataset.name || null)" style="padding:0.4rem 0.9rem;background:var(--accent-color);border:none;border-radius:4px;color:white;font-size:0.82rem;cursor:pointer;">${name ? 'Save' : 'Create'}</button>
                <button onclick="document.getElementById('gr-form-container').style.display='none'" style="padding:0.4rem 0.9rem;background:transparent;border:1px solid var(--border-color);border-radius:4px;color:var(--text-secondary);font-size:0.82rem;cursor:pointer;">Cancel</button>
            </div>
        </div>`;
};

window.submitGRForm = async function(name) {
    const split = s => s.split(',').map(v => v.trim()).filter(Boolean);
    const body = {
        name: document.getElementById('gr-name').value.trim(),
        uriPattern: document.getElementById('gr-pattern').value.trim(),
        permittedActions: split(document.getElementById('gr-actions').value),
        permittedAgents: split(document.getElementById('gr-agents').value),
        contextFetcher: document.getElementById('gr-fetcher').value,
        description: document.getElementById('gr-desc').value.trim(),
        soakMode: document.getElementById('gr-soak').checked,
    };

    const method = name ? 'PUT' : 'POST';
    const url = name ? `/api/governed-resources/${encodeURIComponent(name)}` : '/api/governed-resources';
    const resp = await apiFetch(url, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
    });

    if (resp.ok) {
        document.getElementById('gr-form-container').style.display = 'none';
        loadGovernedResources();
    } else {
        const text = await resp.text();
        alert('Failed: ' + text);
    }
};

window.deleteGR = async function(name) {
    if (!confirm(`Delete GovernedResource "${name}"?`)) return;
    const resp = await apiFetch(`/api/governed-resources/${encodeURIComponent(name)}`, { method: 'DELETE' });
    if (resp.ok || resp.status === 204) {
        loadGovernedResources();
    } else if (resp.status === 409) {
        alert('Cannot delete: active requests are referencing this GovernedResource.');
    } else {
        const text = await resp.text();
        alert('Delete failed: ' + text);
    }
};

// ── Safety Policies tab (G-6, admin only) ────────────────────────────────────

async function loadSafetyPolicies() {
    const tbody = document.getElementById('sp-list');
    const ns = document.getElementById('sp-ns-input')?.value.trim() || 'default';
    try {
        const resp = await apiFetch(`/api/safety-policies?namespace=${encodeURIComponent(ns)}`);
        if (!resp.ok) {
            tbody.innerHTML = `<tr><td colspan="5" style="text-align:center;padding:2rem;color:var(--error);">Failed to load (HTTP ${resp.status})</td></tr>`;
            return;
        }
        const items = (await resp.json()).items || [];
        if (items.length === 0) {
            tbody.innerHTML = `<tr><td colspan="5" style="text-align:center;padding:2rem;color:var(--text-secondary);">No safety policies found in namespace "${escapeHtml(ns)}".</td></tr>`;
            return;
        }
        tbody.innerHTML = items.map(sp => {
            const spName = sp.metadata?.name || sp.name || '';
            const spNs = sp.metadata?.namespace || sp.namespace || ns;
            const rules = sp.spec?.rules || sp.rules || [];
            return `
            <tr>
                <td style="font-family:'JetBrains Mono',monospace;">${escapeHtml(spName)}</td>
                <td><span class="chip">${escapeHtml(spNs)}</span></td>
                <td><span class="chip">${escapeHtml(sp.spec?.contextType || sp.contextType || '')}</span></td>
                <td style="font-size:0.8rem;">${rules.length} rule${rules.length !== 1 ? 's' : ''}</td>
                <td>
                    <button class="details-btn" data-name="${escapeHtml(spName)}" data-ns="${escapeHtml(spNs)}" onclick="openSPForm(this.dataset.name, this.dataset.ns)" style="margin-right:0.4rem;">Edit</button>
                    <button class="details-btn" data-name="${escapeHtml(spName)}" data-ns="${escapeHtml(spNs)}" onclick="deleteSP(this.dataset.name, this.dataset.ns)" style="color:var(--error);">Delete</button>
                </td>
            </tr>`;
        }).join('');
    } catch (e) {
        tbody.innerHTML = `<tr><td colspan="5" style="text-align:center;padding:2rem;color:var(--error);">Error: ${escapeHtml(e.message)}</td></tr>`;
    }
}

window.openSPForm = async function(name, ns) {
    const container = document.getElementById('sp-form-container');
    container.style.display = 'block';
    const activeNs = ns || document.getElementById('sp-ns-input')?.value.trim() || 'default';

    let existing = null;
    if (name) {
        const resp = await apiFetch(`/api/safety-policies/${encodeURIComponent(name)}?namespace=${encodeURIComponent(activeNs)}`);
        if (resp.ok) existing = await resp.json();
    }
    const spec = existing?.spec || existing || {};
    const rules = spec.rules || [];
    const rulesJSON = JSON.stringify(rules, null, 2);

    container.innerHTML = `
        <div style="max-width:700px;">
            <h4 style="margin:0 0 0.75rem;">${name ? 'Edit' : 'Create'} Safety Policy</h4>
            <div style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:0.5rem;margin-bottom:0.5rem;">
                <label style="font-size:0.82rem;color:var(--text-secondary);">Name *
                    <input id="sp-name" type="text" value="${escapeHtml(name || '')}" ${name ? 'readonly' : ''}
                        style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;${name ? 'opacity:0.6;' : ''}"/>
                </label>
                <label style="font-size:0.82rem;color:var(--text-secondary);">Namespace
                    <input id="sp-ns" type="text" value="${escapeHtml(activeNs)}"
                        style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;"/>
                </label>
                <label style="font-size:0.82rem;color:var(--text-secondary);">Context Type
                    <input id="sp-ctx" type="text" value="${escapeHtml(spec.contextType || '')}" placeholder="karpenter"
                        style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.35rem 0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.82rem;"/>
                </label>
            </div>
            <label style="font-size:0.82rem;color:var(--text-secondary);display:block;margin-bottom:0.75rem;">Rules (JSON array)
                <textarea id="sp-rules" rows="6" style="display:block;width:100%;box-sizing:border-box;margin-top:0.2rem;padding:0.5rem;background:var(--surface-color);border:1px solid var(--border-color);border-radius:4px;color:var(--text-primary);font-size:0.8rem;font-family:'JetBrains Mono',monospace;resize:vertical;"></textarea>
            </label>
            <div style="display:flex;gap:0.5rem;">
                <button data-name="${escapeHtml(name || '')}" onclick="submitSPForm(this.dataset.name || null)" style="padding:0.4rem 0.9rem;background:var(--accent-color);border:none;border-radius:4px;color:white;font-size:0.82rem;cursor:pointer;">${name ? 'Save' : 'Create'}</button>
                <button onclick="document.getElementById('sp-form-container').style.display='none'" style="padding:0.4rem 0.9rem;background:transparent;border:1px solid var(--border-color);border-radius:4px;color:var(--text-secondary);font-size:0.82rem;cursor:pointer;">Cancel</button>
            </div>
        </div>`;

    // Set textarea as textContent to avoid XSS
    document.getElementById('sp-rules').textContent = rulesJSON;
};

window.submitSPForm = async function(name) {
    let rules;
    try {
        rules = JSON.parse(document.getElementById('sp-rules').value);
    } catch (_) {
        alert('Rules field is not valid JSON.');
        return;
    }
    const ns = document.getElementById('sp-ns').value.trim() || 'default';
    const body = {
        name: document.getElementById('sp-name').value.trim(),
        namespace: ns,
        contextType: document.getElementById('sp-ctx').value.trim(),
        governedResourceSelector: {},
        rules,
    };

    const method = name ? 'PUT' : 'POST';
    const url = name
        ? `/api/safety-policies/${encodeURIComponent(name)}?namespace=${encodeURIComponent(ns)}`
        : `/api/safety-policies?namespace=${encodeURIComponent(ns)}`;
    const resp = await apiFetch(url, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
    });

    if (resp.ok) {
        document.getElementById('sp-form-container').style.display = 'none';
        loadSafetyPolicies();
    } else {
        const text = await resp.text();
        alert('Failed: ' + text);
    }
};

window.deleteSP = async function(name, ns) {
    if (!confirm(`Delete SafetyPolicy "${name}" in namespace "${ns}"?`)) return;
    const resp = await apiFetch(`/api/safety-policies/${encodeURIComponent(name)}?namespace=${encodeURIComponent(ns)}`, { method: 'DELETE' });
    if (resp.ok || resp.status === 204) {
        loadSafetyPolicies();
    } else {
        const text = await resp.text();
        alert('Delete failed: ' + text);
    }
};

// ── Modals (P0) ──────────────────────────────────────────────────────────────

window.openModal = function(title, bodyHtml, footerHtml = '') {
    previousActiveElement = document.activeElement;
    const overlay = document.getElementById('modal-overlay');
    const content = overlay.querySelector('.modal-content');
    const appRoot = document.getElementById('app-root');

    document.getElementById('modal-title').textContent = title;
    document.getElementById('modal-body').innerHTML = bodyHtml;
    document.getElementById('modal-footer').innerHTML = footerHtml;

    overlay.style.display = 'flex';
    document.body.style.overflow = 'hidden';
    if (appRoot) appRoot.setAttribute('aria-hidden', 'true');

    // Focus management: trap focus inside modal
    const focusableElements = content.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])');
    const firstFocusable = focusableElements[0];
    const lastFocusable = focusableElements[focusableElements.length - 1];

    if (firstFocusable) {
        // Small delay to ensure DOM is ready for focus
        setTimeout(() => firstFocusable.focus(), 50);
    }

    modalKeydownHandler = function(e) {
        if (e.key === 'Escape') {
            closeModal();
        }
        if (e.key === 'Tab') {
            if (e.shiftKey) { // shift + tab
                if (document.activeElement === firstFocusable) {
                    lastFocusable.focus();
                    e.preventDefault();
                }
            } else { // tab
                if (document.activeElement === lastFocusable) {
                    firstFocusable.focus();
                    e.preventDefault();
                }
            }
        }
    };
    document.addEventListener('keydown', modalKeydownHandler);
};

window.closeModal = function() {
    const overlay = document.getElementById('modal-overlay');
    const appRoot = document.getElementById('app-root');

    overlay.style.display = 'none';
    document.body.style.overflow = '';
    if (appRoot) appRoot.removeAttribute('aria-hidden');

    document.removeEventListener('keydown', modalKeydownHandler);

    if (previousActiveElement) {
        previousActiveElement.focus();
    }
};

// Specialized modals

window.viewDiagnosticDetails = function(name) {
    const diag = state.diagnostics.find(d => d.metadata.name === name);
    if (!diag) return;

    // Include correlationID in the modal view (same as the inline panel did)
    const detailsObj = { ...(diag.spec.details || {}) };
    if (diag.spec.correlationID) detailsObj.correlationID = diag.spec.correlationID;
    const json = JSON.stringify(detailsObj, null, 2);
    const body = `
        <div style="margin-bottom: 1rem; font-size: 0.9rem;">
            <strong>Agent:</strong> <span class="chip">${escapeHtml(diag.spec.agentIdentity)}</span>
            <span style="margin-left: 1rem;"><strong>Type:</strong> <span class="badge ${getDiagnosticTypeClass(diag.spec.diagnosticType)}">${escapeHtml(diag.spec.diagnosticType)}</span></span>
        </div>
        <div style="margin-bottom: 0.5rem; font-size: 0.85rem; color: var(--text-secondary);">Raw JSON Data:</div>
        <pre class="details-json-modal" id="modal-json-content">${escapeHtml(json)}</pre>
    `;

    const footer = `
        <button class="copy-btn" onclick="copyToClipboard(event, 'modal-json-content')">Copy to Clipboard</button>
        <button onclick="closeModal()" style="padding: 0.5rem 1rem; background: transparent; border: 1px solid var(--border-color); border-radius: 4px; color: var(--text-secondary); cursor: pointer;">Close</button>
    `;

    openModal('Diagnostic Details', body, footer);
};

window.gradeAgentRequest = function(name) {
    const req = state.requests.find(r => r.metadata.name === name);
    if (!req) return;

    const body = `
        <div style="margin-bottom: 1.5rem; padding: 1rem; background: rgba(255,255,255,0.02); border: 1px solid var(--border-color); border-radius: 8px;">
            <div style="font-size: 0.75rem; text-transform: uppercase; color: var(--text-secondary); margin-bottom: 0.25rem;">Intent Reasoning</div>
            <div style="font-size: 0.9rem; color: #cbd5e1; font-family: monospace;">${escapeHtml(req.spec.reason)}</div>
        </div>

        <div style="margin-bottom: 1rem;">
            <label style="display: block; font-size: 0.85rem; color: var(--text-secondary); margin-bottom: 0.5rem;">Verdict</label>
            <select id="modal-verdict" onchange="toggleReasonCode(this.value)" style="width: 100%; padding: 0.6rem; background: var(--surface-color); border: 1px solid var(--border-color); color: var(--text-primary); border-radius: 4px;">
                <option value="">— select verdict —</option>
                <option value="correct">Correct</option>
                <option value="partial">Partial</option>
                <option value="incorrect">Incorrect</option>
            </select>
        </div>

        <div id="reason-code-container" style="margin-bottom: 1rem; display: none;">
            <label style="display: block; font-size: 0.85rem; color: var(--text-secondary); margin-bottom: 0.5rem;">Reason Code</label>
            <select id="modal-reason-code" style="width: 100%; padding: 0.6rem; background: var(--surface-color); border: 1px solid var(--border-color); color: var(--text-primary); border-radius: 4px;">
                <option value="wrong_diagnosis">Wrong Diagnosis (affects accuracy)</option>
                <option value="bad_timing">Bad Timing</option>
                <option value="scope_too_broad">Scope Too Broad</option>
                <option value="precautionary">Precautionary</option>
                <option value="policy_block">Policy Block</option>
            </select>
        </div>

        <div>
            <label style="display: block; font-size: 0.85rem; color: var(--text-secondary); margin-bottom: 0.5rem;">Reviewer Note (optional)</label>
            <textarea id="modal-note" rows="4" maxlength="512" style="width: 100%; box-sizing: border-box; padding: 0.6rem; background: var(--surface-color); border: 1px solid var(--border-color); color: var(--text-primary); border-radius: 4px; resize: vertical;"></textarea>
        </div>
    `;

    const footer = `
        <button onclick="closeModal()" style="padding: 0.5rem 1.25rem; background: transparent; border: 1px solid var(--border-color); border-radius: 4px; color: var(--text-secondary); cursor: pointer;">Cancel</button>
        <button onclick="submitAgentRequestVerdict('${escapeHtml(name)}')" style="padding: 0.5rem 1.25rem; background: var(--accent-color); border: none; border-radius: 4px; color: white; font-weight: 600; cursor: pointer;">Submit Grade</button>
    `;

    openModal('Grade Agent Request', body, footer);
};

window.toggleReasonCode = function(verdict) {
    const container = document.getElementById('reason-code-container');
    if (verdict === 'partial' || verdict === 'incorrect') {
        container.style.display = 'block';
    } else {
        container.style.display = 'none';
    }
};

window.submitAgentRequestVerdict = async function(name) {
    const verdict = document.getElementById('modal-verdict').value;
    const reasonCode = document.getElementById('modal-reason-code').value;
    const note = document.getElementById('modal-note').value.trim();
    
    if (!verdict) {
        document.getElementById('modal-verdict').style.borderColor = 'var(--error)';
        return;
    }

    closeModal();
    const req = state.requests.find(r => r.metadata.name === name);
    if (!req) {
        alert('Request not found: ' + name);
        return;
    }
    const ns = req.metadata.namespace || 'default';

    try {
        const response = await apiFetch(`/api/agent-requests/${encodeURIComponent(name)}/verdict?namespace=${encodeURIComponent(ns)}`, {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ verdict, reasonCode: (verdict === 'correct' ? '' : reasonCode), note })
        });
        if (!response.ok) {
            const errText = await response.text();
            alert('Failed to submit verdict: ' + errText);
            return;
        }
        await fetchRequests();
    } catch (e) {
        alert('Failed to submit verdict: ' + e.message);
    }
};

window.gradeDiagnostic = function(name) {
    const diag = state.diagnostics.find(d => d.metadata.name === name);
    if (!diag) return;

    const verdict = diag.status?.verdict || '';
    const note = diag.status?.reviewerNote || '';

    const body = `
        <div style="margin-bottom: 1.5rem; padding: 1rem; background: rgba(255,255,255,0.02); border: 1px solid var(--border-color); border-radius: 8px;">
            <div style="font-size: 0.75rem; text-transform: uppercase; color: var(--text-secondary); margin-bottom: 0.25rem;">Diagnostic Summary</div>
            <div style="font-size: 1rem; color: var(--text-primary);">${escapeHtml(diag.spec.summary)}</div>
        </div>

        <div style="margin-bottom: 1rem;">
            <label style="display: block; font-size: 0.85rem; color: var(--text-secondary); margin-bottom: 0.5rem;">Verdict</label>
            <select id="modal-verdict" style="width: 100%; padding: 0.6rem; background: var(--surface-color); border: 1px solid var(--border-color); color: var(--text-primary); border-radius: 4px;">
                <option value="" ${!verdict ? 'selected' : ''}>— select verdict —</option>
                <option value="correct" ${verdict === 'correct' ? 'selected' : ''}>Correct</option>
                <option value="partial" ${verdict === 'partial' ? 'selected' : ''}>Partial</option>
                <option value="incorrect" ${verdict === 'incorrect' ? 'selected' : ''}>Incorrect</option>
            </select>
        </div>

        <div>
            <label style="display: block; font-size: 0.85rem; color: var(--text-secondary); margin-bottom: 0.5rem;">Reviewer Note (optional)</label>
            <textarea id="modal-note" rows="4" maxlength="512" style="width: 100%; box-sizing: border-box; padding: 0.6rem; background: var(--surface-color); border: 1px solid var(--border-color); color: var(--text-primary); border-radius: 4px; resize: vertical;">${escapeHtml(note)}</textarea>
        </div>
    `;

    const footer = `
        <button onclick="closeModal()" style="padding: 0.5rem 1.25rem; background: transparent; border: 1px solid var(--border-color); border-radius: 4px; color: var(--text-secondary); cursor: pointer;">Cancel</button>
        <button onclick="submitModalReview('${escapeHtml(name)}')" style="padding: 0.5rem 1.25rem; background: var(--accent-color); border: none; border-radius: 4px; color: white; font-weight: 600; cursor: pointer;">Submit Grade</button>
    `;

    openModal('Grade Diagnostic', body, footer);
};

window.submitModalReview = async function(name) {
    const verdict = document.getElementById('modal-verdict').value;
    const note = document.getElementById('modal-note').value.trim();
    if (!verdict) {
        document.getElementById('modal-verdict').style.borderColor = 'var(--error)';
        return;
    }

    closeModal();
    const ns = state.namespace;
    try {
        const response = await apiFetch(`/api/agent-diagnostics/${encodeURIComponent(name)}/status?namespace=${encodeURIComponent(ns)}`, {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ verdict, reviewerNote: note })
        });
        if (!response.ok) {
            const errText = await response.text();
            alert('Failed to submit review: ' + errText);
            return;
        }
        await loadDiagnostics();
    } catch (e) {
        alert('Failed to submit review: ' + e.message);
    }
};

window.confirmDenial = function(name) {
    const req = state.requests.find(r => r.metadata.name === name);
    if (!req) return;

    const body = `
        <div style="margin-bottom: 1rem;">
            <h3 style="color: var(--error); margin-bottom: 0.5rem;">Confirm Denial</h3>
            <p style="font-size: 0.9rem; color: var(--text-secondary);">
                You are about to deny the request from <strong>${escapeHtml(req.spec.agentIdentity)}</strong> to 
                <strong>${escapeHtml(req.spec.action)}</strong> on <code>${escapeHtml(req.spec.target.uri)}</code>.
            </p>
        </div>
        <label style="display: block; font-size: 0.85rem; color: var(--text-secondary); margin-bottom: 0.5rem;">Reason for denial (optional)</label>
        <textarea id="deny-reason" rows="3" placeholder="e.g., resources are currently at capacity"
            style="width: 100%; box-sizing: border-box; padding: 0.6rem; background: var(--surface-color); border: 1px solid var(--border-color); color: var(--text-primary); border-radius: 4px; resize: vertical;"></textarea>
    `;

    const footer = `
        <button onclick="closeModal()" style="padding: 0.5rem 1.25rem; background: transparent; border: 1px solid var(--border-color); border-radius: 4px; color: var(--text-secondary); cursor: pointer;">Cancel</button>
        <button onclick="submitDenial('${escapeHtml(name)}')" style="padding: 0.5rem 1.25rem; background: var(--error); border: none; border-radius: 4px; color: white; font-weight: 600; cursor: pointer;">Deny Request</button>
    `;

    openModal('Deny Agent Request', body, footer);
};

window.submitDenial = async function(name) {
    const reason = document.getElementById('deny-reason').value.trim();
    closeModal();
    await performAction(name, 'deny', reason);
};

window.copyToClipboard = function(evt, elementId) {
    if (evt) {
        evt.preventDefault();
        evt.stopPropagation();
    }
    const text = document.getElementById(elementId).innerText;
    const btn = evt ? evt.target : null;

    const onSuccess = () => {
        if (btn) {
            const oldText = btn.textContent;
            btn.textContent = 'Copied!';
            setTimeout(() => btn.textContent = oldText, 2000);
        }
    };

    const onFailure = (err) => {
        console.error('Copy to clipboard failed:', err);
        if (btn) {
            const oldText = btn.textContent;
            btn.textContent = 'Failed to copy';
            btn.style.color = 'var(--error)';
            setTimeout(() => {
                btn.textContent = oldText;
                btn.style.color = '';
            }, 2000);
        }
    };

    // Try modern API
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(onSuccess).catch(err => {
            // If modern API fails (e.g. permission), try fallback
            fallbackCopy(text) ? onSuccess() : onFailure(err);
        });
    } else {
        // No modern API, use fallback
        fallbackCopy(text) ? onSuccess() : onFailure(new Error('Clipboard API unavailable'));
    }
};

function fallbackCopy(text) {
    const textArea = document.createElement('textarea');
    textArea.value = text;
    // Ensure textarea is not visible but part of DOM
    textArea.style.position = 'fixed';
    textArea.style.left = '-9999px';
    textArea.style.top = '0';
    document.body.appendChild(textArea);
    textArea.focus();
    textArea.select();
    let successful = false;
    try {
        successful = document.execCommand('copy');
    } catch (err) {
        successful = false;
    }
    document.body.removeChild(textArea);
    return successful;
}

// ── Bootstrap ────────────────────────────────────────────────────────────────

async function init() {
    await loadIdentity();
    if (!getToken() && !state.proxyAuth) {
        showBanner('Not authenticated — paste a Bearer token to continue.', 'warn');
    }
    fetchRequests();
}

init();
setInterval(fetchRequests, 3000);
setInterval(loadDiagnostics, 3000);