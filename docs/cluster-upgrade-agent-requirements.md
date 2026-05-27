# Cluster Upgrade Agent — Requirements Discovery

This document captures the questions we need answered before designing the cluster
upgrade agent feature. Please fill in or discuss each section with your team before
our next call.

---

## 1. Upgrade Scope

**1.1** What is the current Kubernetes version across your clusters, and what is the
target version you want to upgrade to?

> Example: "We are on 1.33 today and want to reach 1.34."

**1.2** Is this a one-time upgrade or an ongoing need — i.e., do you want a reusable
agent that handles every future upgrade cycle?

**1.3** How many clusters are in scope? Are they all on the same version today, or do
you have a mixed fleet?

**1.4** Are control-plane upgrades and node pool/worker node upgrades both in scope, or
just the control plane?

**1.5** Are cluster add-ons (CoreDNS, kube-proxy, CNI plugin, cluster autoscaler) in
scope for the upgrade agent, or handled separately?

---

## 2. Cluster Infrastructure

**2.1** Which Kubernetes distribution(s) are you running?

- [ ] Amazon EKS
- [ ] Google GKE
- [ ] Azure AKS
- [ ] Rancher / RKE2
- [ ] kubeadm (self-managed)
- [ ] OpenShift
- [ ] Other: _______________

**2.2** Do you have a mix of distributions across your fleet, or is it uniform?

**2.3** Do all clusters live in the same cloud account/project/subscription, or are
they spread across multiple accounts or regions?

**2.4** Do any clusters have custom API server flags, admission webhooks, or
non-standard configurations that could affect upgrade compatibility?

---

## 3. Pre-flight Checks

**3.1** Should the agent perform pre-flight checks before the upgrade, after, or both?

**3.2** Which of the following checks do you want the agent to run? *(select all that apply)*

- [ ] **Deprecated API usage** — identify workloads using API versions removed in the
  target release (e.g., `flowcontrol.apiserver.k8s.io/v1beta2` removed in 1.29)
- [ ] **PodDisruptionBudget violations** — identify PDBs that would block node drain
  during upgrade (e.g., `maxUnavailable: 0` with single-replica deployments)
- [ ] **Resource quota headroom** — verify nodes have sufficient resource budget to
  tolerate cordon/drain during rolling upgrade
- [ ] **Node readiness** — confirm all nodes are healthy before upgrade begins
- [ ] **Pending pods** — identify pods stuck in Pending or CrashLoopBackOff
- [ ] **Webhook compatibility** — check admission webhooks are compatible with the
  target version
- [ ] **etcd health** — verify etcd cluster is healthy before control-plane upgrade
- [ ] **Other**: _______________

**3.3** If the agent finds issues (deprecated APIs, offending PDBs, etc.), what should
happen?

- [ ] Report findings only — surface to a human, block the upgrade until resolved
- [ ] Report and auto-remediate — agent fixes what it can (e.g., updates deprecated
  manifests), reports what it cannot
- [ ] Block upgrade unconditionally until all findings are resolved
- [ ] Proceed with upgrade and log findings for post-upgrade review

**3.4** Are there checks you consider mandatory blockers vs. warnings? For example:
deprecated API usage is a hard block, but a pending pod is just a warning.

---

## 4. Agent Autonomy and Approval

**4.1** Should the agent be able to trigger the upgrade autonomously after passing
pre-flight checks, or must a human always approve before the upgrade starts?

**4.2** If human approval is required, who in your organisation approves cluster
upgrades? Is there a single approver or does it vary by environment (dev vs. prod)?

**4.3** Should approval requirements differ by environment?

> Example: dev clusters auto-approved, staging requires one approval, production
> requires two.

**4.4** Do you want a canary or wave-based upgrade strategy — upgrading one cluster
first, validating, then proceeding to the rest?

**4.5** If an upgrade fails mid-flight, should the agent attempt an automatic rollback,
or alert a human and halt?

---

## 5. Remediation

**5.1** If the agent finds deprecated API usage, what should it do?

- [ ] Report only — give a list of affected workloads to the team
- [ ] Suggest fixes — provide updated manifests for human review and apply
- [ ] Auto-update — update the manifests directly in the cluster without human
  intervention
- [ ] Open a ticket / PR — create a GitHub PR or Jira ticket with the fix

**5.2** If the agent finds an offending PDB (one that would block node drain), what
should it do?

- [ ] Report only — list the offending PDBs to the team
- [ ] Temporarily relax the PDB during upgrade, restore after
- [ ] Require the team to fix the PDB before proceeding
- [ ] Auto-patch the PDB during the maintenance window

**5.3** Are there workload owners who need to be notified before the agent modifies
anything in their namespace? How is namespace ownership tracked today?

**5.4** Are there any workloads that must never be auto-remediated — for example,
stateful workloads, databases, or security-critical components?

---

## 6. Policy and Governance

**6.1** Do you want upgrade policies codified in a control plane (like AIP) so that
agents cannot bypass them — even if the agent is compromised or misbehaves?

**6.2** Which constraints do you want to enforce as non-bypassable policies?

> Examples:
> - Never skip a minor version (1.33 → 1.35 without 1.34 is always blocked)
> - Production upgrades only during a defined maintenance window
> - Upgrade blocked if deprecated API violations are present
> - Minimum cluster health score required before upgrade

**6.3** Do you have existing maintenance windows for cluster operations? If so, what
are they?

> Example: "Weeknights 10pm–2am Pacific, Saturday 8am–4pm Pacific for production."

**6.4** Should the policies differ per cluster tier (dev / staging / production /
critical)?

**6.5** Do you have compliance requirements that mandate a full audit trail of who
triggered the upgrade, what pre-flight checks were run, who approved, and when?

**6.6** Are there external systems (change management, ticketing, ITSM) that must be
notified or create a change record before an upgrade proceeds?

---

## 7. Notifications and Observability

**7.1** Where should the agent send findings and status updates?

- [ ] Slack / Teams
- [ ] PagerDuty / OpsGenie
- [ ] Email
- [ ] Jira / ServiceNow ticket
- [ ] Dashboard only (AIP dashboard)
- [ ] Other: _______________

**7.2** Who should receive notifications — the platform team only, or also workload
owners in affected namespaces?

**7.3** Do you need post-upgrade validation — the agent verifying the cluster is healthy
and all workloads are running after the upgrade completes?

---

## 8. Existing Tooling

**8.1** Are you already using any tools for upgrade preparation today?

> Examples: `pluto` (deprecated API scanner), `kubectl convert`, `nova` (Helm chart
> upgrade advisor), `kube-no-trouble (kubent)`.

**8.2** Do you have existing CI/CD pipelines that trigger cluster upgrades today? If so,
where does this agent fit — does it replace that pipeline or complement it?

**8.3** Do you use GitOps (Flux, ArgoCD) for cluster configuration? If so, deprecated
API fixes would need to land in Git before the upgrade — is that flow in scope?

---

## 9. Success Criteria

**9.1** What does a successful outcome look like for you? For example:

- "Agent runs pre-flight checks, blocks upgrades with deprecated APIs, requires human
  approval for production, and produces an audit log."
- "Fully autonomous upgrades on dev/staging, human-gated on production."

**9.2** What is the timeline? Is there a specific K8s version end-of-life deadline
driving this?

**9.3** Are there clusters that need to be upgraded urgently (CVE or EOL pressure)?

---

## Notes / Open Items

| # | Item | Owner | Due |
|---|------|-------|-----|
| | | | |
| | | | |
