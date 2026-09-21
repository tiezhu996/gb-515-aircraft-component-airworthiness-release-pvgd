
export interface DomainRecord {
  id: number;
  code: string;
  name: string;
  status: string;
  version: number;
  description: string;
  facility: string;
  owner: string;
  category: string;
  riskLevel: 'low' | 'medium' | 'high' | 'critical';
  metricValue: number;
  metricUnit: string;
  effectiveAt: string;
  evidence: string;
  relatedCode: string;
	preparedBy?: string;
	verifiedBy?: string;
	submittedBy?: string;
	reviewedBy?: string;
	reviewReason?: string;
	/** 检查任务失败时记录的判定原因，重新检查通过后清空 */
	failureReason?: string;
	/** 部件失败阻断前的状态，用于一次性恢复 */
	preBlockStatus?: string;
	/** 触发适航阻断的失败检查任务编号 */
	blockedByTaskCode?: string;
	/** 阻断原因（失败检查任务的判定原因） */
	blockReason?: string;
	blockedAt?: string | null;
	blockResolvedAt?: string | null;
	revisions?: VersionRevision[];
	createdAt: string;
	updatedAt: string;
}

export interface VersionRevision {
  id: number;
  version: number;
  status: string;
  evidence: string;
  actor: string;
  requestId: string;
  action: string;
  reason: string;
  createdAt: string;
}

export interface PageMeta { page: number; pageSize: number; total: number }
export interface ApiEnvelope<T> { data: T; error?: string; message?: string; meta?: PageMeta }
export interface UserSession { token: string; username: string; displayName: string; role: string; expiresIn: number }
export interface AuditLog {
  id: number; requestId: string; actor: string; action: string; entityType: string;
  entityId: number; beforeState: string; afterState: string; detail: string; createdAt: string;
}
export interface EntityConfig {
  key: string;
  path: string;
  label: string;
  statuses: readonly string[];
  primaryTransitions: Readonly<Record<string, string>>;
}
