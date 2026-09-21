
import { useEffect, useMemo, useState } from 'react';
import type { DomainRecord } from '../types/domain';
import type { EntityConfig } from '../types/status';
import type { EntityStore } from '../stores/factory';
import { formatDate } from '../utils/format';
import { StatusBadge } from './common/StatusBadge';
import { PartStatusBadge } from './common/PartStatusBadge';
import { MetricCard } from './common/MetricCard';
import { ConfirmDialog } from './common/ConfirmDialog';
import { UiButton } from './common/UiButton';
import { CertificatePanel } from './common/CertificatePanel';
import { useAuth } from '../hooks/useAuth';

interface PendingTransition { item: DomainRecord; status: string; reason: string; tone: 'normal' | 'danger' }

export function EntityPage({ config, useStore, certificateRecords = [] }: { config: EntityConfig; useStore: EntityStore; certificateRecords?: DomainRecord[] }) {
	const { items, meta, loading, error, load, createRecord, transition } = useStore();
	const { session, hasRole } = useAuth();
  const [search, setSearch] = useState('');
  const [showCreate, setShowCreate] = useState(false);
  const [pending, setPending] = useState<PendingTransition | null>(null);
  useEffect(() => { void load(config.path); }, [config.path, load]);
  const highRisk = useMemo(() => items.filter((item) => ['high', 'critical'].includes(item.riskLevel)).length, [items]);
  const blockedCount = useMemo(() => items.filter((item) => item.blockActive).length, [items]);
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
		(config.key === 'releaseAuthorization' && target !== 'review');
	const canAdvance = (item: DomainRecord, target: string) => canOperate && (!requiresReviewer(item, target) || hasRole('reviewer'));
	const usePartBadge = config.key === 'aircraftPart' || config.key === 'releaseAuthorization';

	// Airworthiness block guards mirrored from the backend state machine: a part
	// cannot be released and an authorization cannot be re-submitted/approved
	// while a failed inspection for the component code is still unresolved.
	const blockedByFailure = (item: DomainRecord): boolean => item.blockActive === true;
	const blockNote = (item: DomainRecord) => item.blockingTaskCode
		? `检查任务 ${item.blockingTaskCode} 判定失败${item.blockActive ? '待复核' : '已重新检查通过'}：${item.blockingReason || '未填写失败原因'}`
		: '';

	const transitionLabel = (target: string) => ({
		failed: '判定失败', passed: '判定通过', released: '放行', approved: '批准',
		restricted: '限制放行', review: '提交复核', revoked: '撤销', hold: '转暂停',
		running: '开始/重新检查', valid: '发布', expired: '置过期', retired: '退役', inspection: '转检查中',
	}[target] || `推进至 ${target}`);

	const requestTransition = (item: DomainRecord, target: string) => {
		const isFailure = config.key === 'inspectionTask' && target === 'failed';
		const reason = isFailure
			? `检查任务 ${item.code} 判定失败：经复核确认的适航不符合项，需重新检查后再提交复核`
			: '前端工作台人工确认';
		setPending({ item, status: target, reason, tone: isFailure ? 'danger' : 'normal' });
	};

	const renderActions = (item: DomainRecord) => {
		const primary = config.primaryTransitions[item.status];
		const secondary = config.secondaryTransitions?.[item.status];
		if (blockedByFailure(item)) {
			return <span className="muted">适航阻断中，等待重新检查</span>;
		}
		const buttons = [];
		if (primary && canAdvance(item, primary)) {
			const danger = primary === 'failed';
			buttons.push(<button key="primary" className={danger ? 'table-action table-action--danger' : 'table-action'} onClick={() => requestTransition(item, primary)}>{transitionLabel(primary)}</button>);
		}
		if (secondary && canAdvance(item, secondary)) {
			buttons.push(<button key="secondary" className="table-action table-action--secondary" onClick={() => requestTransition(item, secondary)}>{transitionLabel(secondary)}</button>);
		}
		if (buttons.length > 0) return <span className="action-group">{buttons}</span>;
		if (primary && requiresReviewer(item, primary) && role === 'operator') return <span className="muted">等待复核员</span>;
		if (primary && !canOperate) return <span className="muted">只读</span>;
		return <span className="muted">流程结束</span>;
	};

	return <main className="workspace">
		<header className="page-header"><div><p className="eyebrow">业务工作台</p><h1>{config.label}</h1><p>统一管理{config.label}的状态、风险、证据与责任人。</p></div>{canOperate ? <UiButton onClick={() => setShowCreate(true)}>新增{config.label}</UiButton> : <span className="access-note">只读权限</span>}</header>
		<section className="metrics"><MetricCard label="记录总数" value={meta.total} detail="当前筛选范围"/><MetricCard label="高风险" value={highRisk} detail="需要优先复核"/>{blockedCount > 0 ? <MetricCard label="适航阻断" value={blockedCount} detail="失败检查待复核"/> : <MetricCard label="状态种类" value={new Set(items.map((item) => item.status)).size} detail="状态机覆盖"/>}</section>
		{certificateRecords.length > 0 && <section className="certificate-section"><header><h2>证书版本证据</h2><span>操作者与请求 ID 可追溯</span></header><CertificatePanel records={certificateRecords} /></section>}
		<section className="toolbar"><input aria-label="搜索" placeholder={`搜索${config.label}编码或名称`} value={search} onChange={(event) => setSearch(event.target.value)} /><UiButton onClick={() => void load(config.path, search)}>查询</UiButton><button className="link-button" onClick={() => { setSearch(''); void load(config.path); }}>重置</button></section>
    {error && <div className="alert" role="alert">{error}</div>}
    <section className="table-shell" aria-busy={loading}><table><thead><tr><th>编码</th><th>名称</th><th>状态</th><th>风险</th><th>责任人</th><th>指标</th><th>更新时间</th><th>操作</th></tr></thead><tbody>
			{items.map((item) => <tr key={item.id} className={item.blockActive ? 'row-blocked' : undefined}>
				<td><strong>{item.code}</strong>{item.blockActive && <span className="block-flag" title={blockNote(item)}>适航阻断</span>}</td>
				<td>{item.name}<small>{item.facility}</small>
					{config.key === 'inspectionTask' && item.status === 'failed' && item.failureReason && <small className="block-detail">失败原因：{item.failureReason}</small>}
					{item.blockActive && <small className="block-detail">{blockNote(item)}</small>}
					{!item.blockActive && item.blockingTaskCode && <small className="block-detail block-detail--recovered">{blockNote(item)}</small>}
				</td>
				<td>{usePartBadge ? <PartStatusBadge status={item.status}/> : <StatusBadge status={item.status}/>}</td>
				<td>{item.riskLevel}</td><td>{item.owner}</td><td>{item.metricValue} {item.metricUnit}</td><td>{formatDate(item.updatedAt)}</td>
				<td>{renderActions(item)}</td>
			</tr>)}
      {!items.length && !loading && <tr><td colSpan={8} className="empty">暂无记录</td></tr>}
    </tbody></table>{loading && <div className="loading">正在同步业务数据…</div>}</section>
		<ConfirmDialog open={showCreate} title={`新增${config.label}`} onCancel={() => setShowCreate(false)} onConfirm={() => void createDemo().catch(() => undefined)}><p>将创建一条包含完整责任人、风险和证据信息的演示记录。</p></ConfirmDialog>
		<ConfirmDialog open={Boolean(pending)} title={pending?.tone === 'danger' ? '确认检查失败判定' : '确认状态迁移'} onCancel={() => setPending(null)} onConfirm={() => { if (pending) void transition(config.path, pending.item, pending.status, pending.reason).then(() => setPending(null)).catch(() => undefined); }}>
			{pending?.tone === 'danger'
				? <><p>判定失败后将立即形成适航阻断闭环：同编号部件转暂停、待复核/已批准授权转限制放行，并记录失败任务编号与原因。</p><p>阻断期间不得继续放行或批准；重新检查通过后，授权只能重新提交双人复核。</p></>
				: <p>状态迁移会写入不可覆盖的版本与审计日志。</p>}
			<strong>{pending?.item.status} → {pending?.status}</strong>
		</ConfirmDialog>
	</main>;
}
