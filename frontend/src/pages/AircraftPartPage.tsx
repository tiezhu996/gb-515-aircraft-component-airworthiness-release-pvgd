
import { useEffect } from 'react';
import { EntityPage } from '../components/EntityPage';
import { ENTITY_CONFIGS } from '../types/status';
import { useAircraftPartStore } from '../stores/aircraft-part';
import { useInspectionTaskStore } from '../stores/inspection-task';
export default function AircraftPartPage() {
  const inspections = useInspectionTaskStore((state) => state.items);
  const loadInspections = useInspectionTaskStore((state) => state.load);
  useEffect(() => { void loadInspections('inspections'); }, [loadInspections]);
  return <EntityPage config={ENTITY_CONFIGS[0]} useStore={useAircraftPartStore} inspectionRecords={inspections} />;
}
