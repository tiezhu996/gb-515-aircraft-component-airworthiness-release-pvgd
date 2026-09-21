
import { useEffect, useMemo, useState } from 'react';
import type { EntityConfig, DomainRecord } from '../types/domain';
import type { EntityStore } from '../stores/factory';
import { nextStatus, formatDate } from '../utils/format';
import { StatusBadge } from './common/StatusBadge';
import { PartStatusBadge } from './common/PartStatusBadge';
import { MetricCard } from './common/MetricCard';
import { ConfirmDialog } from './common/ConfirmDialog';
import { UiButton } from './common/UiButton';
import { CertificatePanel } from './common/CertificatePanel';
import { BlockNotice } from './common/BlockNotice';
import { useAuth } from '../hooks/useAuth';
import { useFailedInspections } from '../hooks/useInspectionBlocks';

export function EntityPage({ config, useStore, certificateRecords = [], inspectionRecords = [] }: { config: EntityConfig; useStore: EntityStore; certificateRecords?: DomainRecord[]; inspectionRecords?: DomainRecord[] }) {
	const { items, meta, loading, error, load, createRecord, transition } = useStore();
	const { session, hasRole } = useAuth();
  const [search, setSearch] = useState('');
  const [showCreate, setShowCreate] = useState(false);
  const [pending, setPending] = useState<{ item: DomainRecord; status: string; reason: string } | null>(null);
  useEffect(() => { void load(config.path); }, [config.path, load]);
  const highRisk = useMemo(() => items.filter((item) => ['high', 'critical'].includes(item.riskLevel)).length, [items]);
  // Failed inspections shared across the part/inspection/authorization pages;
  // the map is keyed by the shared RelatedCode.
  const failedInspections = useFailedInspections(inspectionRecords);
  const createDemo = async () => {
    const now = Date.now();
    await createRecord(config.path, { code: `${config.key.toUpperCase()}-${now.toString().slice(-6)}`, name: `新增${config.label}`,
      description: '通过前端工作台创建的业务记录', facility: '默认作业区', owner: '现场操作员', category: '常规', riskLevel: 'medium',
      metricValue: 25, metricUnit: 'unit', effectiveAt: new Date().toISOString(), evidence: '已完成创建前检查', relatedCode: '' });
    setShowCreate(false);
  };
	const role = session?.role || 'viewer';
	const canOperate = hasRole('operator');
	const requiresReviewer = (item: DomainRecord, target: string) =>
		(config.key === 'certificateRecord' && ['valid', 'revoked'].includes(target)) ||
		(config.key === 'releaseAuthorization' && item.status !== 'draft' && !(item.status === 'restricted' && target === 'review'));
	const canAdvance = (item: DomainRecord, target: string) => canOperate && (!requiresReviewer(item, target) || hasRole('reviewer'));
	const usePartBadge = config.key === 'aircraftPart' || config.key === 'releaseAuthorization';

	const relatedBlock = (item: DomainRecord) => failedInspections.get((item.relatedCode || '').trim().toUpperCase());
	const isBlocked = (item: DomainRecord): boolean => {
		if (item.blockedAt && !item.blockResolvedAt) return true;
		if (config.key === 'releaseAuthorization' && item.status === 'restricted') return Boolean(relatedBlock(item));
		return false;
	};

	// Determine the allowed actions. Running inspections offer both passed and
	// failed decisions; the failed path triggers the airworthiness cascade.
	// A restricted authorization can be resubmitted for review only after the
	// failure block is resolved.
	type AdvanceOption = { target: string; label: string; reason: string; danger: boolean };
	const advanceOptions = (item: DomainRecord): AdvanceOption[] => {
		if (config.key === 'inspectionTask' && item.status === 'running') {
			return [
				{ target: 'passed', label: '判定通过', reason: '检查任务判定通过', danger: false },
				{ target: 'failed', label: '判定失败', reason: '检查任务判定失败，触发适航阻断', danger: true },
			];
		}
		if (config.key === 'releaseAuthorization' && item.status === 'restricted') {
			const openBlock = relatedBlock(item);
			// Resubmission is offered only when this authorization was blocked
			// by a failed inspection, that block was resolved by a passed
			// re-inspection, and no other failed inspection covers the code.
			if (item.blockedByTaskCode && item.blockResolvedAt && !openBlock) {
				return [{ target: 'review', label: '重新提交复核', reason: '重新检查已通过，授权重新提交双人复核', danger: false }];
			}
			// Reviewer-decided restriction keeps the original revoke workflow.
			if (!item.blockedByTaskCode && hasRole('reviewer')) {
				return [{ target: 'revoked', label: '推进至 revoked', reason: '前端工作台人工确认', danger: false }];
			}
			return [];
		}
		const target = nextStatus(item.status, config.primaryTransitions);
		return target ? [{ target, label: `推进至 ${target}`, reason: '前端工作台人工确认', danger: false }] : [];
	};

	const renderStatusCell = (item: DomainRecord) => {
		const badge = usePartBadge ? <PartStatusBadge status={item.status}/> : <StatusBadge status={item.status}/>;
		if (config.key === 'inspectionTask' && item.status === 'failed') {
			return <>{badge}<BlockNotice taskCode={item.code} reason={item.failureReason}/></>;
		}
		if (config.key === 'aircraftPart' && item.blockedByTaskCode) {
			return <>{badge}<BlockNotice taskCode={item.blockedByTaskCode} reason={item.blockReason} resolved={Boolean(item.blockResolvedAt)}/></>;
		}
		if (config.key === 'releaseAuthorization' && item.blockedByTaskCode) {
			return <>{badge}<BlockNotice taskCode={item.blockedByTaskCode} reason={item.blockReason} resolved={Boolean(item.blockResolvedAt)}/></>;
		}
		return badge;
	};

	const renderActionCell = (item: DomainRecord) => {
		// Paused part while a block is active: no manual move is possible.
		if (config.key === 'aircraftPart' && isBlocked(item)) {
			return <span className="muted">失败检查未通过，暂停中</span>;
		}
		const options = advanceOptions(item);
		if (config.key === 'releaseAuthorization' && item.status === 'restricted' && isBlocked(item)) {
			// Failure-blocked and still open: release and approval are closed.
			return <span className="muted">失败检查未通过，禁止放行</span>;
		}
		if (!options.length) {
			if (!canOperate) return <span className="muted">只读</span>;
			return <span className="muted">流程结束</span>;
		}
		const allowed = options.filter((option) => canAdvance(item, option.target));
		if (allowed.length) {
			return <span className="action-group">{allowed.map((option) =>
				<button key={option.target} className={option.danger ? 'table-action table-action--danger' : 'table-action'} onClick={() => setPending({ item, status: option.target, reason: option.reason })}>{option.label}</button>,
			)}</span>;
		}
		const waiting = options.some((option) => requiresReviewer(item, option.target) && role === 'operator');
		if (waiting) return <span className="muted">等待复核员</span>;
		if (!canOperate) return <span className="muted">只读</span>;
		return <span className="muted">流程结束</span>;
	};

	return <main className="workspace">
		<header className="page-header"><div><p className="eyebrow">业务工作台</p><h1>{config.label}</h1><p>统一管理{config.label}的状态、风险、证据与责任人。</p></div>{canOperate ? <UiButton onClick={() => setShowCreate(true)}>新增{config.label}</UiButton> : <span className="access-note">只读权限</span>}</header>
		<section className="metrics"><MetricCard label="记录总数" value={meta.total} detail="当前筛选范围"/><MetricCard label="高风险" value={highRisk} detail="需要优先复核"/><MetricCard label="状态种类" value={new Set(items.map((item) => item.status)).size} detail="状态机覆盖"/></section>
		{certificateRecords.length > 0 && <section className="certificate-section"><header><h2>证书版本证据</h2><span>操作者与请求 ID 可追溯</span></header><CertificatePanel records={certificateRecords} /></section>}
		<section className="toolbar"><input aria-label="搜索" placeholder={`搜索${config.label}编码或名称`} value={search} onChange={(event) => setSearch(event.target.value)} /><UiButton onClick={() => void load(config.path, search)}>查询</UiButton><button className="link-button" onClick={() => { setSearch(''); void load(config.path); }}>重置</button></section>
    {error && <div className="alert" role="alert">{error}</div>}
    <section className="table-shell" aria-busy={loading}><table><thead><tr><th>编码</th><th>名称</th><th>状态</th><th>风险</th><th>责任人</th><th>指标</th><th>更新时间</th><th>操作</th></tr></thead><tbody>
			{items.map((item) => { return <tr key={item.id} className={isBlocked(item) ? 'row-blocked' : undefined}><td><strong>{item.code}</strong>{item.relatedCode ? <small>编号 {item.relatedCode}</small> : null}</td><td>{item.name}<small>{item.facility}</small></td><td>{renderStatusCell(item)}</td><td>{item.riskLevel}</td><td>{item.owner}</td><td>{item.metricValue} {item.metricUnit}</td><td>{formatDate(item.updatedAt)}</td><td>{renderActionCell(item)}</td></tr>; })}
      {!items.length && !loading && <tr><td colSpan={8} className="empty">暂无记录</td></tr>}
    </tbody></table>{loading && <div className="loading">正在同步业务数据…</div>}</section>
		<ConfirmDialog open={showCreate} title={`新增${config.label}`} onCancel={() => setShowCreate(false)} onConfirm={() => void createDemo().catch(() => undefined)}><p>将创建一条包含完整责任人、风险和证据信息的演示记录。</p></ConfirmDialog>
		<ConfirmDialog open={Boolean(pending)} title="确认状态迁移" onCancel={() => setPending(null)} onConfirm={() => { if (pending) void transition(config.path, pending.item, pending.status, pending.reason).then(() => setPending(null)).catch(() => undefined); }}><p>状态迁移会写入不可覆盖的版本与审计日志。</p><strong>{pending?.item.status} → {pending?.status}</strong></ConfirmDialog>
	</main>;
}
