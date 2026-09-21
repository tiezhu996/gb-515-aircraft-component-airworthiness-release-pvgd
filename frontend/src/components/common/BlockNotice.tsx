interface BlockNoticeProps {
  taskCode: string;
  reason?: string;
  resolved?: boolean;
}

/**
 * Renders the failed inspection task code and reason on parts, inspections
 * and authorizations so the airworthiness blocking relationship stays
 * consistent after a page refresh.
 */
export function BlockNotice({ taskCode, reason, resolved = false }: BlockNoticeProps) {
  return <small className={resolved ? 'block-notice block-notice--resolved' : 'block-notice'}>
    <strong>{resolved ? '阻断已解除' : '适航阻断'}</strong>
    <span>失败任务 {taskCode}</span>
    {reason ? <em title={reason}>{reason}</em> : null}
  </small>;
}
