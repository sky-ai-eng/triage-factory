package domain

import "testing"

// allAccessActions is every discriminator the audit log writes. The category
// buckets must cover it exactly once: an action in neither bucket vanishes the
// moment a viewer narrows the log, which is worse than noise — the filtered view
// reads as "nothing happened" rather than "not shown here".
var allAccessActions = []string{
	AccessActionOrgMemberGranted,
	AccessActionOrgRoleChanged,
	AccessActionOrgMemberRevoked,
	AccessActionOrgOwnershipTransferred,
	AccessActionTeamMemberAdded,
	AccessActionTeamRoleChanged,
	AccessActionTeamMemberRemoved,
	AccessActionInviteCreated,
	AccessActionInviteRevoked,
	AccessActionCredentialSet,
	AccessActionCredentialRemoved,
	AccessActionEventSourceDisabled,
	AccessActionEventSourceEnabled,
	AccessActionAPITokenCreated,
	AccessActionAPITokenRevoked,
	AccessActionSSOConnectionCreated,
	AccessActionSSOConnectionEnabled,
	AccessActionSSOConnectionDisabled,
	AccessActionSSOEnforcementEnabled,
	AccessActionSSOEnforcementDisabled,
	AccessActionSSODomainClaimed,
	AccessActionSSODomainVerified,
	AccessActionSSODomainRemoved,
	AccessActionSSOBreakGlassAdded,
	AccessActionSSOBreakGlassRemoved,
}

func TestAccessActionsInCategory_PartitionsEveryAction(t *testing.T) {
	seen := map[string]int{}
	for _, cat := range []string{AccessCategoryMembership, AccessCategoryCredential, AccessCategoryPolicy} {
		for _, a := range AccessActionsInCategory(cat) {
			seen[a]++
		}
	}
	for _, a := range allAccessActions {
		switch seen[a] {
		case 1:
		case 0:
			t.Errorf("action %q belongs to no category — a filtered view would hide it", a)
		default:
			t.Errorf("action %q is in %d categories, want exactly 1", a, seen[a])
		}
	}
	if len(seen) != len(allAccessActions) {
		t.Errorf("categories name %d distinct actions, want %d — an action constant is missing from this test or a category classifies an unknown one",
			len(seen), len(allAccessActions))
	}
}

func TestAccessActionsInCategory_UnknownMeansNoFilter(t *testing.T) {
	for _, c := range []string{"", "bogus"} {
		if got := AccessActionsInCategory(c); got != nil {
			t.Errorf("AccessActionsInCategory(%q) = %v, want nil (no filter — every action)", c, got)
		}
	}
}
