
import { EntityPage } from '../components/EntityPage';
import { ENTITY_CONFIGS } from '../types/status';
import { useReleaseAuthorizationStore } from '../stores/release-authorization';
import { useEffect } from 'react';
import { useInspectionTaskStore } from '../stores/inspection-task';
export default function ReleaseAuthorizationPage() {
  const inspections = useInspectionTaskStore((state) => state.items);
  const loadInspections = useInspectionTaskStore((state) => state.load);
  useEffect(() => { void loadInspections('inspections'); }, [loadInspections]);
  return <EntityPage config={ENTITY_CONFIGS[3]} useStore={useReleaseAuthorizationStore} inspectionRecords={inspections} />;
}
