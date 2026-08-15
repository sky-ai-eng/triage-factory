// Moved to src/ui/shared/useFocusTrap.ts.
//
// The primitive knows nothing about Triage Factory — it is Tab containment,
// initial focus and focus restore over a ref — so by the design system's own
// test it belongs there, and `Dialog` needs it from inside src/ui/ where the
// import boundary forbids reaching back into hooks/.
//
// Re-exported rather than moved outright because seventeen feature components
// import it from here, and rewriting those imports is churn on files that are
// being replaced anyway.
export { useFocusTrap } from '../ui/shared/useFocusTrap'
