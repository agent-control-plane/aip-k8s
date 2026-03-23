const state = {
    requests: [],
    selectedRequest: null,
    auditRecords: []
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
        const response = await fetch(`/api/audit-records?agentRequest=${name}`);
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
        const response = await fetch(`/api/agent-requests/${name}/${action}`, opts);
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
// If the control plane verified live endpoints, a reason is mandatory —
// the human must justify overriding independent cluster state evidence.
function promptApproval(name, hasActiveEndpoints) {
    if (!hasActiveEndpoints) {
        performAction(name, 'approve', '');
        return;
    }

    // Build the reason modal
    const overlay = document.createElement('div');
    overlay.id = 'reason-overlay';
    overlay.style.cssText = `
        position:fixed; inset:0; background:rgba(0,0,0,0.7); z-index:1000;
        display:flex; align-items:center; justify-content:center;`;

    overlay.innerHTML = `
        <div style="background:var(--surface-color);border:1px solid var(--border-color);border-radius:8px;padding:2rem;max-width:480px;width:90%;box-shadow:0 8px 32px rgba(0,0,0,0.5);">
            <h3 style="margin:0 0 0.5rem;color:var(--error);">⚠ Override Required — Live Traffic Detected</h3>
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
                <button onclick="submitApprovalWithReason('${name}')"
                    style="padding:0.5rem 1.25rem;background:var(--error);border:none;
                           border-radius:4px;color:white;font-weight:600;cursor:pointer;">Confirm Override</button>
            </div>
        </div>`;

    document.body.appendChild(overlay);
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
            <div class="request-item ${isActive ? 'active' : ''}" onclick="selectRequestById('${req.metadata.name}')">
                <div class="title">${req.spec.agentIdentity}</div>
                <div class="meta">
                    <span class="badge badge-${phase.toLowerCase()}">${phase}</span>
                    <span>${time}</span>
                </div>
                <div style="font-size: 0.75rem; color: var(--text-secondary); margin-top: 0.4rem;">
                    ${req.spec.action} → ${req.spec.target.uri}
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
    // Semantically correct labels per condition type
    const positiveTypes = ['PolicyEvaluated', 'Approved', 'LockAcquired', 'Executing', 'Completed'];
    const isPositive = positiveTypes.includes(condition.type);
    const isTrue = condition.status === 'True';

    let color, label;
    if (condition.type === 'RequiresApproval' && isTrue) {
        color = 'var(--warning)';
        label = 'PENDING REVIEW';
    } else if (isPositive && isTrue) {
        color = 'var(--success)';
        label = 'TRUE';
    } else if (!isPositive && isTrue) {
        color = 'var(--error)';
        label = 'TRUE';
    } else {
        color = 'var(--text-secondary)';
        label = 'FALSE';
    }
    return `<span style="color: ${color}; font-weight: 600; font-size: 0.8rem;">${label}</span>`;
}

function renderBlastRadius(cascadeModel) {
    if (!cascadeModel || !cascadeModel.affectedTargets || cascadeModel.affectedTargets.length === 0) {
        return `<div style="color: var(--text-secondary); font-size: 0.85rem;">No blast radius declared</div>`;
    }

    const effectColors = {
        disrupted: 'var(--error)',
        modified: 'var(--warning)',
        deleted: '#ff4444',
        orphaned: 'var(--text-secondary)'
    };

    return cascadeModel.affectedTargets.map(t => {
        const color = effectColors[t.effectType] || 'var(--text-secondary)';
        return `
            <div style="display: flex; align-items: center; gap: 0.75rem; padding: 0.5rem; background: rgba(255,255,255,0.03); border-radius: 4px; margin-bottom: 0.5rem;">
                <span style="color: ${color}; font-weight: 700; font-size: 0.7rem; text-transform: uppercase; min-width: 70px;">${t.effectType}</span>
                <span style="font-family: 'JetBrains Mono', monospace; font-size: 0.8rem; color: #cbd5e1;">${t.uri}</span>
            </div>
        `;
    }).join('');
}

function renderReasoningTrace(rt) {
    const confidence = rt.confidenceScore ?? null;
    const pct = confidence !== null ? Math.round(confidence * 100) : null;
    const color = pct === null ? 'var(--text-secondary)' : pct >= 80 ? 'var(--success)' : pct >= 60 ? 'var(--warning)' : 'var(--error)';

    const componentRows = rt.componentConfidence
        ? Object.entries(rt.componentConfidence).map(([k, v]) => {
            const p = Math.round(parseFloat(v) * 100);
            const c = p >= 80 ? 'var(--success)' : p >= 60 ? 'var(--warning)' : 'var(--error)';
            return `
                <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom:0.4rem; font-size:0.82rem;">
                    <span style="color:var(--text-secondary);">${k.replace(/_/g,' ')}</span>
                    <span style="color:${c}; font-weight:600;">${p}%</span>
                </div>`;
        }).join('') : '';

    const alternatives = rt.alternatives?.length
        ? rt.alternatives.map(a => `<span style="display:inline-block; padding:0.2rem 0.6rem; background:rgba(255,255,255,0.05); border-radius:9999px; font-size:0.78rem; margin:0.2rem 0.2rem 0 0; color:var(--text-secondary);">${a}</span>`).join('')
        : '<span style="color:var(--text-secondary); font-size:0.82rem;">None declared</span>';

    const traceLink = rt.traceReference
        ? `<div style="margin-top:0.75rem; font-size:0.78rem;"><span style="color:var(--text-secondary);">Trace: </span><span style="font-family:monospace; color:var(--accent-color);">${rt.traceReference}</span></div>`
        : '';

    return `
        <div class="panel" style="margin-top:1.5rem;">
            <div class="panel-title">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 19c-5 1.5-5-2.5-7-3m14 6v-3.87a3.37 3.37 0 0 0-.94-2.61c3.14-.35 6.44-1.54 6.44-7A5.44 5.44 0 0 0 20 4.77 5.07 5.07 0 0 0 19.91 1S18.73.65 16 2.48a13.38 13.38 0 0 0-7 0C6.27.65 5.09 1 5.09 1A5.07 5.07 0 0 0 5 4.77a5.44 5.44 0 0 0-1.5 3.78c0 5.42 3.3 6.61 6.44 7A3.37 3.37 0 0 0 9 18.13V22"></path></svg>
                Agent Reasoning Trace
            </div>
            ${pct !== null ? `
                <div style="display:flex; align-items:center; gap:1rem; margin-bottom:1rem;">
                    <span style="font-size:0.85rem; color:var(--text-secondary);">Overall confidence</span>
                    <span style="font-size:1.4rem; font-weight:700; color:${color};">${pct}%</span>
                    <div class="confidence-meter" style="flex:1;"><div class="confidence-fill" style="width:${pct}%; background:${color};"></div></div>
                </div>
                ${componentRows ? `<div style="margin-bottom:1rem;">${componentRows}</div>` : ''}
            ` : ''}
            <div style="font-size:0.82rem; color:var(--text-secondary); margin-bottom:0.4rem; text-transform:uppercase; letter-spacing:0.05em;">Alternatives considered</div>
            <div style="margin-bottom:0.5rem;">${alternatives}</div>
            ${traceLink}
        </div>`;
}

function renderParameters(params) {
    if (!params || Object.keys(params).length === 0) return '';
    const rows = Object.entries(params).map(([k, v]) => {
        const isFalse = v === false;
        const color = isFalse ? 'var(--error)' : 'var(--text-primary)';
        return `
            <div style="display:flex; justify-content:space-between; padding:0.4rem 0; border-bottom:1px solid var(--border-color); font-size:0.85rem;">
                <span style="color:var(--text-secondary);">${k}</span>
                <span style="font-family:monospace; color:${color};">${JSON.stringify(v)}</span>
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
        return `<div style="color:var(--text-secondary);font-size:0.82rem;padding:0.5rem 0;">
            No verification data yet — pending evaluation.
        </div>`;
    }

    const check  = `<span style="color:var(--error);font-weight:700;">✗</span>`;
    const ok     = `<span style="color:var(--success);font-weight:700;">✓</span>`;
    const endpointIcon = cpv.hasActiveEndpoints ? check : ok;
    const replicaIcon  = cpv.readyReplicas > 1   ? check : ok;

    const rows = [
        { icon: cpv.targetExists ? ok : check,  label: 'Target exists',       value: cpv.targetExists ? 'yes' : 'not found' },
        { icon: endpointIcon,                    label: 'Active endpoints',    value: cpv.hasActiveEndpoints ? `${cpv.activeEndpointCount} detected` : 'none' },
        { icon: replicaIcon,                     label: 'Ready replicas',      value: `${cpv.readyReplicas} / ${cpv.specReplicas} spec` },
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
                <span style="color:var(--error);font-weight:700;">✗</span>
                <span style="font-family:'JetBrains Mono',monospace;color:#cbd5e1;">${s}</span>
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
        ${fetched ? `<div style="margin-top:0.6rem;font-size:0.7rem;color:var(--text-secondary);">Verified at ${fetched}</div>` : ''}
    `;
}

function renderGovernanceTimeline(phase, needsApproval) {
    const steps = [
        { label: 'Intent declared',  done: true },
        { label: 'Policy evaluated', done: true },
        { label: 'Human gate',       done: phase === 'Approved' || phase === 'Denied' || phase === 'Executing' || phase === 'Completed', active: needsApproval, denied: phase === 'Denied' },
        { label: 'Action executed',  done: phase === 'Completed', active: phase === 'Executing' },
    ];

    const stepHtml = steps.map((s, i) => {
        let dotColor, dotContent, labelColor;
        if (s.denied) {
            dotColor = 'var(--error)'; dotContent = '✕'; labelColor = 'var(--error)';
        } else if (s.active) {
            dotColor = 'var(--warning)'; dotContent = '◉'; labelColor = 'var(--warning)';
        } else if (s.done) {
            dotColor = 'var(--success)'; dotContent = '●'; labelColor = 'var(--text-secondary)';
        } else {
            dotColor = 'var(--border-color)'; dotContent = '○'; labelColor = 'var(--border-color)';
        }

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
            </div>
        `;
        return;
    }

    const phase = req.status?.phase || 'Pending';
    // Sort ascending (oldest first) for timeline readability
    const auditLogs = [...state.auditRecords].sort((a, b) => new Date(a.spec.timestamp) - new Date(b.spec.timestamp));

    const needsApproval = phase === 'Pending' && req.status?.conditions?.some(c => c.type === 'RequiresApproval' && c.status === 'True');
    const reason = req.spec.reason || 'No reason provided.';

    const cascadeCount = req.spec.cascadeModel?.affectedTargets?.length || 0;

    detailsEl.innerHTML = `
        <div style="display: flex; justify-content: space-between; align-items: flex-start;">
            <div>
                <h2 style="margin-bottom: 0.5rem;">${req.spec.agentIdentity}</h2>
                <div style="color: var(--text-secondary); font-size: 0.9rem;">
                    ${req.metadata.name} &nbsp;·&nbsp; ${req.spec.action} on <code style="font-size: 0.85rem;">${req.spec.target.uri}</code>
                </div>
            </div>
            <span class="badge badge-${phase.toLowerCase()}" style="font-size: 1rem; padding: 0.5rem 1rem;">${phase}</span>
        </div>

        ${renderGovernanceTimeline(phase, needsApproval)}

        <div class="side-by-side" style="margin-top: 1rem;">
            <div class="panel">
                <div class="panel-title">
                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"></path></svg>
                    Agent Declared
                </div>
                <div class="reasoning-content" style="margin-bottom:1rem;">${reason}</div>
                <div style="font-size:0.78rem; text-transform:uppercase; letter-spacing:0.05em; color:var(--text-secondary); margin-bottom:0.5rem;">Confidence</div>
                ${req.spec.reasoningTrace?.confidenceScore != null ? (() => {
                    const pct = Math.round(req.spec.reasoningTrace.confidenceScore * 100);
                    const color = pct >= 80 ? 'var(--success)' : pct >= 60 ? 'var(--warning)' : 'var(--error)';
                    return `<div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:1rem;">
                        <span style="font-size:1.3rem;font-weight:700;color:${color};">${pct}%</span>
                        <div class="confidence-meter" style="flex:1;"><div class="confidence-fill" style="width:${pct}%;background:${color};"></div></div>
                    </div>`;
                })() : '<div style="color:var(--text-secondary);font-size:0.82rem;margin-bottom:1rem;">Not declared</div>'}
                <div style="font-size:0.78rem; text-transform:uppercase; letter-spacing:0.05em; color:var(--text-secondary); margin-bottom:0.5rem;">Downstream (Causal Model)</div>
                ${req.spec.cascadeModel?.affectedTargets?.length
                    ? req.spec.cascadeModel.affectedTargets.map(t => {
                        const effectColors = { disrupted:'var(--error)', modified:'var(--warning)', deleted:'#ff4444', orphaned:'var(--text-secondary)' };
                        const color = effectColors[t.effectType] || 'var(--text-secondary)';
                        return `<div style="display:flex;align-items:center;gap:0.5rem;padding:0.3rem 0;font-size:0.8rem;">
                            <span style="color:${color};font-weight:700;min-width:65px;font-size:0.7rem;text-transform:uppercase;">${t.effectType}</span>
                            <span style="font-family:'JetBrains Mono',monospace;color:#cbd5e1;">${t.uri.split('/').pop()}</span>
                        </div>`;
                    }).join('')
                    : '<div style="color:var(--text-secondary);font-size:0.82rem;">None declared</div>'}
                ${req.spec.cascadeModel?.modelSourceTrust
                    ? `<div style="margin-top:0.75rem;font-size:0.75rem;color:var(--text-secondary);">Model source trust: <strong style="color:var(--text-primary);">${req.spec.cascadeModel.modelSourceTrust}</strong>${req.spec.cascadeModel.modelSourceId ? ' · ' + req.spec.cascadeModel.modelSourceId : ''}</div>`
                    : ''}
            </div>

            <div class="panel">
                <div class="panel-title" style="color: var(--warning);">
                    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"></path></svg>
                    Control Plane Verified
                </div>
                ${renderControlPlaneVerification(req.status?.controlPlaneVerification)}
                <div style="margin-top:1.25rem;">
                    <div style="font-size:0.78rem; text-transform:uppercase; letter-spacing:0.05em; color:var(--text-secondary); margin-bottom:0.5rem;">Policy Conditions</div>
                    ${req.status?.conditions?.map(c => `
                        <div style="margin-bottom: 0.6rem; padding: 0.4rem 0.5rem; background: rgba(255,255,255,0.03); border-radius: 4px;">
                            <div style="display: flex; justify-content: space-between; font-size: 0.82rem;">
                                <span style="font-weight: 600;">${c.type}</span>
                                ${conditionBadge(c)}
                            </div>
                            <div style="font-size: 0.72rem; color: var(--text-secondary); margin-top: 0.15rem;">${c.message || ''}</div>
                        </div>
                    `).join('') || '<div style="color: var(--text-secondary); font-size: 0.85rem;">No conditions yet</div>'}
                </div>
                ${needsApproval ? `
                    <div class="actions">
                        <button class="primary" onclick="promptApproval('${req.metadata.name}', ${!!req.status?.controlPlaneVerification?.hasActiveEndpoints})">Approve Override</button>
                        <button class="danger" onclick="performAction('${req.metadata.name}', 'deny')">Deny</button>
                    </div>
                ` : ''}
            </div>
        </div>

        ${req.spec.reasoningTrace?.alternatives?.length || req.spec.reasoningTrace?.componentConfidence ? renderReasoningTrace(req.spec.reasoningTrace) : ''}

        ${req.spec.parameters ? renderParameters(req.spec.parameters) : ''}

        <div class="audit-logs" style="margin-top: 1.5rem;">
            <h3 style="margin-bottom: 1rem;">Audit Trail</h3>
            ${auditLogs.length === 0
                ? '<div style="color: var(--text-secondary); font-size: 0.85rem;">No audit records yet</div>'
                : auditLogs.map(log => `
                    <div class="audit-item">
                        <span class="time">${new Date(log.spec.timestamp).toLocaleTimeString()}</span>
                        <span style="font-weight: 600; color: var(--accent-color);">${log.spec.event}</span>
                        ${log.spec.phaseTransition ? `
                            <span style="color: var(--text-secondary);">&nbsp;(${log.spec.phaseTransition.from} → ${log.spec.phaseTransition.to})</span>
                        ` : ''}
                    </div>
                `).join('')
            }
        </div>
    `;
}

fetchRequests();
setInterval(fetchRequests, 3000);
