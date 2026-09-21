
import { statusTone } from '../../utils/format';

const labels: Record<string, string> = {
  planned: '已计划', running: '检查中', passed: '检查通过', failed: '判定失败',
  draft: '草稿', valid: '有效', expired: '已过期', revoked: '已撤销',
};

export function StatusBadge({ status }: { status: string }) {
  return <span className={`status status--${statusTone(status)}`}>{labels[status] || status.replaceAll('_', ' ')}</span>;
}
