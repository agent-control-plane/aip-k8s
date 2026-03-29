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

const state = {
    requests: [],
    selectedRequest: null,
    auditRecords: [],
    diagnostics: [],
    namespace: 'default'
};

async function fetchRequests() {
    try {
        const response = await fetch('/api/agent-requests');
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
    try {
        const response = await fetch(`/api/audit-records?agentRequest=${encodeURIComponent(name)}`);
        state.auditRecords = await response.json();
        renderDetails();
    } catch (err) {
        console.error('Failed to fetch audit records:', err);
    }
}

async function performAction(name, action, reason) {
    try {
        const opts = { method: 'POST' };
        if (reason !== undefined) {
            opts.headers = { 'Content-Type': 'application/json' };
            opts.body = JSON.stringify({ reason });
        }
        const response = await fetch(`/api/agent-requests/${encodeURIComponent(name)}/${encodeURIComponent(action)}`, opts);
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
    // Bind name via closure, not inline onclick, to avoid injection.
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

function renderGovernanceTimeline(phase, needsApproval) {
    const steps = [
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
    const reason = req.spec.reason || 'No reason provided.';
    const name = req.metadata.name;

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

        ${renderGovernanceTimeline(phase, needsApproval)}

        <div class="side-by-side" style="margin-top:1rem;">
            <div class="panel">
                <div class="panel-title">
                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"></path></svg>
                    Agent Declared
                </div>
                <div class="reasoning-content" style="margin-bottom:1rem;">${escapeHtml(reason)}</div>
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
                <div style="margin-top:1.25rem;">
                    <div style="font-size:0.78rem;text-transform:uppercase;letter-spacing:0.05em;color:var(--text-secondary);margin-bottom:0.5rem;">Policy Conditions</div>
                    ${req.status?.conditions?.map(c => `
                        <div style="margin-bottom:0.6rem;padding:0.4rem 0.5rem;background:rgba(255,255,255,0.03);border-radius:4px;">
                            <div style="display:flex;justify-content:space-between;font-size:0.82rem;">
                                <span style="font-weight:600;">${escapeHtml(c.type)}</span>
                                ${conditionBadge(c)}
                            </div>
                            <div style="font-size:0.72rem;color:var(--text-secondary);margin-top:0.15rem;">${escapeHtml(c.message || '')}</div>
                        </div>
                    `).join('') || '<div style="color:var(--text-secondary);font-size:0.85rem;">No conditions yet</div>'}
                </div>
                ${needsApproval ? `
                    <div class="actions">
                        <button class="primary" data-name="${escapeHtml(name)}" data-endpoints="${!!req.status?.controlPlaneVerification?.hasActiveEndpoints}" onclick="promptApproval(this.dataset.name, this.dataset.endpoints === 'true')">Approve Override</button>
                        <button class="danger" data-name="${escapeHtml(name)}" onclick="performAction(this.dataset.name, 'deny')">Deny</button>
                    </div>
                ` : ''}
            </div>
        </div>

        ${req.spec.reasoningTrace?.alternatives?.length || req.spec.reasoningTrace?.componentConfidence ? renderReasoningTrace(req.spec.reasoningTrace) : ''}
        ${req.spec.parameters ? renderParameters(req.spec.parameters) : ''}

        <div class="audit-logs" style="margin-top:1.5rem;">
            <h3 style="margin-bottom:1rem;">Audit Trail</h3>
            ${auditLogs.length === 0
                ? '<div style="color:var(--text-secondary);font-size:0.85rem;">No audit records yet</div>'
                : auditLogs.map(log => `
                    <div class="audit-item">
                        <span class="time">${new Date(log.spec.timestamp).toLocaleTimeString()}</span>
                        <span style="font-weight:600;color:var(--accent-color);">${escapeHtml(log.spec.event)}</span>
                        ${log.spec.phaseTransition ? `
                            <span style="color:var(--text-secondary);">&nbsp;(${escapeHtml(log.spec.phaseTransition.from)} &rarr; ${escapeHtml(log.spec.phaseTransition.to)})</span>
                        ` : ''}
                    </div>
                `).join('')}
        </div>`;
}

// ── Diagnostics tab ──────────────────────────────────────────────────────────

window.showTab = function(tabName) {
    const requestsView   = document.getElementById('requests-view');
    const diagnosticsView = document.getElementById('diagnostics-view');
    const tabRequests    = document.getElementById('tab-requests');
    const tabDiagnostics = document.getElementById('tab-diagnostics');

    if (tabName === 'requests') {
        requestsView.style.display   = 'block';
        diagnosticsView.style.display = 'none';
        tabRequests.classList.add('active');
        tabDiagnostics.classList.remove('active');
    } else {
        requestsView.style.display   = 'none';
        diagnosticsView.style.display = 'block';
        tabRequests.classList.remove('active');
        tabDiagnostics.classList.add('active');
        loadDiagnostics();
    }
};

window.loadDiagnostics = async function() {
    try {
        const response = await fetch(`/api/agent-diagnostics?namespace=${encodeURIComponent(state.namespace)}`);
        if (!response.ok) throw new Error('Failed to fetch diagnostics');
        state.diagnostics = await response.json();
        renderDiagnostics();
    } catch (err) {
        console.error('Error fetching diagnostics:', err);
    }
};

function renderDiagnostics() {
    const listEl = document.getElementById('diagnostics-list');
    if (!listEl) return;

    if (!state.diagnostics || state.diagnostics.length === 0) {
        listEl.innerHTML = `
            <tr><td colspan="6" style="text-align:center;padding:3rem;color:var(--text-secondary);">
                No diagnostics found in namespace &ldquo;${escapeHtml(state.namespace)}&rdquo;
            </td></tr>`;
        return;
    }

    const sorted = [...state.diagnostics].sort((a, b) =>
        new Date(b.metadata.creationTimestamp) - new Date(a.metadata.creationTimestamp)
    );

    listEl.innerHTML = sorted.map(diag => {
        const age = formatAge(diag.metadata.creationTimestamp);
        const detailsId = `details-${escapeHtml(diag.metadata.name)}`;
        const hasDetails = diag.spec.details && Object.keys(diag.spec.details).length > 0;

        return `
            <tr>
                <td style="white-space:nowrap;color:var(--text-secondary);">${age}</td>
                <td><span class="chip">${escapeHtml(diag.spec.agentIdentity)}</span></td>
                <td><span class="badge ${getDiagnosticTypeClass(diag.spec.diagnosticType)}">${escapeHtml(diag.spec.diagnosticType)}</span></td>
                <td><span class="chip">${escapeHtml(diag.spec.correlationID)}</span></td>
                <td style="max-width:300px;">${escapeHtml(diag.spec.summary)}</td>
                <td>${hasDetails
                    ? `<button class="details-btn" data-target="${escapeHtml(detailsId)}" onclick="toggleDetails(this.dataset.target)">View</button>
                       <div id="${escapeHtml(detailsId)}" class="details-json" style="display:none"></div>`
                    : '<span style="color:var(--text-secondary);font-size:0.8rem;">None</span>'
                }</td>
            </tr>`;
    }).join('');

    // Populate JSON as textContent (not innerHTML) to prevent injection.
    sorted.forEach(diag => {
        if (!diag.spec.details || Object.keys(diag.spec.details).length === 0) return;
        const el = document.getElementById(`details-${diag.metadata.name}`);
        if (el) el.textContent = JSON.stringify(diag.spec.details, null, 2);
    });
}

function getDiagnosticTypeClass(type) {
    const t = (type || '').toLowerCase();
    if (t.includes('error') || t.includes('fail')) return 'badge-failed';
    if (t.includes('warn')) return 'badge-denied';
    if (t.includes('success') || t.includes('observation') || t.includes('diagnosis')) return 'badge-completed';
    return 'badge-executing';
}

window.toggleDetails = function(id) {
    const el = document.getElementById(id);
    if (!el) return;
    el.style.display = el.style.display === 'none' ? 'block' : 'none';
    const btn = el.previousElementSibling;
    if (btn) btn.textContent = el.style.display === 'none' ? 'View' : 'Hide';
};

function formatAge(timestamp) {
    const diff = Math.floor((Date.now() - new Date(timestamp)) / 1000);
    if (diff < 60) return diff + 's';
    if (diff < 3600) return Math.floor(diff / 60) + 'm';
    if (diff < 86400) return Math.floor(diff / 3600) + 'h';
    return Math.floor(diff / 86400) + 'd';
}

// ── Bootstrap ────────────────────────────────────────────────────────────────

fetchRequests();
setInterval(fetchRequests, 3000);
setInterval(loadDiagnostics, 3000);
