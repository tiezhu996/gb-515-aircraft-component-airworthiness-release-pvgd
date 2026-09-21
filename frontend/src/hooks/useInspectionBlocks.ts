import { useMemo } from 'react';
import type { DomainRecord } from '../types/domain';

export interface FailedInspection {
  taskCode: string;
  reason: string;
  updatedAt: string;
}

/**
 * Indexes currently-failed inspection tasks by their shared RelatedCode so
 * the part and authorization pages render the same airworthiness blocking
 * relationship as the inspection page after a refresh. When several failed
 * tasks share a code, the most recently updated one is surfaced.
 */
export function useFailedInspections(inspections: DomainRecord[]): Map<string, FailedInspection> {
  return useMemo(() => {
    const index = new Map<string, FailedInspection>();
    for (const task of inspections) {
      const relatedCode = (task.relatedCode || '').trim().toUpperCase();
      if (!relatedCode || task.status !== 'failed') continue;
      const existing = index.get(relatedCode);
      if (!existing || task.updatedAt > existing.updatedAt) {
        index.set(relatedCode, { taskCode: task.code, reason: task.failureReason || '', updatedAt: task.updatedAt });
      }
    }
    return index;
  }, [inspections]);
}
