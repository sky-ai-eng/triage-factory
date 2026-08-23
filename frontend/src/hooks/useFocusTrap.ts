// The dialog focus-trap primitive, re-exported from the design system for the
// feature components that import it from hooks/. The implementation lives in
// src/ui/shared/ because `Dialog` needs it inside src/ui/, where the import
// boundary forbids reaching back into hooks/.
export { useFocusTrap } from '../ui/shared/useFocusTrap'
