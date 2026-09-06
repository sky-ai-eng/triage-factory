package githubbind

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeGitHub serves the two endpoints the gates read, from a handler the test
// supplies. Every subtest builds its own so one arm's misbehaviour can't leak
// into another's.
func fakeGitHub(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAssociated(t *testing.T) {
	ctx := context.Background()

	t.Run("PresentPasses", func(t *testing.T) {
		base := fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer ghu_test" {
				t.Errorf("Authorization = %q, want the user access token", got)
			}
			fmt.Fprint(w, `{"total_count":2,"installations":[{"id":11},{"id":42}]}`)
		})
		if err := Associated(ctx, base, "ghu_test", 42); err != nil {
			t.Errorf("Associated = %v, want nil", err)
		}
	})

	t.Run("AbsentIsTheDefinitiveNo", func(t *testing.T) {
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"total_count":1,"installations":[{"id":11}]}`)
		})
		err := Associated(ctx, base, "ghu_test", 42)
		if !errors.Is(err, ErrNotAssociated) {
			t.Errorf("Associated = %v, want ErrNotAssociated", err)
		}
	})

	t.Run("EmptyListIsTheDefinitiveNo", func(t *testing.T) {
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"total_count":0,"installations":[]}`)
		})
		if err := Associated(ctx, base, "ghu_test", 42); !errors.Is(err, ErrNotAssociated) {
			t.Errorf("Associated = %v, want ErrNotAssociated", err)
		}
	})

	t.Run("SecondPageIsFollowed", func(t *testing.T) {
		// An installer associated with more than a page of installations must
		// not be refused for arriving on page two.
		var base string
		base = fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"installations":[{"id":42}]}`)
				return
			}
			w.Header().Set("Link", `<`+base+`/user/installations?per_page=100&page=2>; rel="next"`)
			fmt.Fprint(w, `{"installations":[{"id":11}]}`)
		})
		if err := Associated(ctx, base, "ghu_test", 42); err != nil {
			t.Errorf("Associated = %v, want nil across the page boundary", err)
		}
	})

	t.Run("OffHostNextPageRefuses", func(t *testing.T) {
		// The token rides every request, so a Link pointing elsewhere must
		// never be followed — and a truncated listing must never read as
		// "not associated".
		evil := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("the off-host next page was fetched; the user token would have leaked")
			fmt.Fprint(w, `{"installations":[{"id":42}]}`)
		})
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Link", `<`+evil+`/user/installations?page=2>; rel="next"`)
			fmt.Fprint(w, `{"installations":[{"id":11}]}`)
		})
		err := Associated(ctx, base, "ghu_test", 42)
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Associated = %v, want ErrUndetermined", err)
		}
	})

	t.Run("EndlessPaginationRefuses", func(t *testing.T) {
		// A host that answers every page with another rel="next" — a
		// misconfigured GHES, a proxy rewriting Link headers — must not keep a
		// browser waiting indefinitely. The per-request timeout does not bound
		// the loop; the page cap does.
		var (
			base   string
			served int
		)
		base = fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			served++
			w.Header().Set("Link", `<`+base+`/user/installations?page=next>; rel="next"`)
			fmt.Fprint(w, `{"installations":[{"id":11}]}`)
		})
		err := Associated(ctx, base, "ghu_test", 42)
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Associated = %v, want ErrUndetermined", err)
		}
		if errors.Is(err, ErrNotAssociated) {
			t.Error("a listing that never ended must not read as a definitive no")
		}
		if served > maxInstallationPages {
			t.Errorf("served %d pages, want no more than the cap of %d", served, maxInstallationPages)
		}
	})

	t.Run("TransportFailureIsUndetermined", func(t *testing.T) {
		// A GitHub that never answers is not evidence of anything.
		err := Associated(ctx, "http://127.0.0.1:1", "ghu_test", 42)
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Associated = %v, want ErrUndetermined", err)
		}
		if errors.Is(err, ErrNotAssociated) {
			t.Error("a transport failure must not read as a definitive no")
		}
	})

	t.Run("ServerErrorIsUndetermined", func(t *testing.T) {
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		if err := Associated(ctx, base, "ghu_test", 42); !errors.Is(err, ErrUndetermined) {
			t.Errorf("Associated = %v, want ErrUndetermined", err)
		}
	})

	t.Run("MalformedBodyIsUndetermined", func(t *testing.T) {
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `not json`)
		})
		if err := Associated(ctx, base, "ghu_test", 42); !errors.Is(err, ErrUndetermined) {
			t.Errorf("Associated = %v, want ErrUndetermined", err)
		}
	})
}

func TestAssociatedByAccount(t *testing.T) {
	ctx := context.Background()

	t.Run("NamedAccountResolvesToItsInstallation", func(t *testing.T) {
		base := fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer ghu_test" {
				t.Errorf("Authorization = %q, want the user access token", got)
			}
			fmt.Fprint(w, `{"total_count":2,"installations":[{"id":11,"account":{"login":"other"}},{"id":42,"account":{"login":"Acme"}}]}`)
		})
		id, err := AssociatedByAccount(ctx, base, "ghu_test", "acme")
		if err != nil || id != 42 {
			t.Errorf("AssociatedByAccount = (%d, %v), want (42, nil) — logins compare case-insensitively", id, err)
		}
	})

	t.Run("AccountNotInTheListingIsTheDefinitiveNo", func(t *testing.T) {
		// Whether the account has no installation or the user cannot see it,
		// the listing looks the same, and so must the answer.
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"total_count":1,"installations":[{"id":11,"account":{"login":"other"}}]}`)
		})
		id, err := AssociatedByAccount(ctx, base, "ghu_test", "acme")
		if !errors.Is(err, ErrNotAssociated) || id != 0 {
			t.Errorf("AssociatedByAccount = (%d, %v), want (0, ErrNotAssociated)", id, err)
		}
	})

	t.Run("AnEntryWithNoIdIsNotAMatch", func(t *testing.T) {
		// An id of zero would be handed to the App as "read installation 0"
		// and refused there; refusing here keeps the definitive no where the
		// listing is the evidence.
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"total_count":1,"installations":[{"account":{"login":"acme"}}]}`)
		})
		if _, err := AssociatedByAccount(ctx, base, "ghu_test", "acme"); !errors.Is(err, ErrNotAssociated) {
			t.Errorf("AssociatedByAccount = %v, want ErrNotAssociated", err)
		}
	})

	t.Run("SecondPageIsFollowed", func(t *testing.T) {
		var base string
		base = fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"installations":[{"id":42,"account":{"login":"acme"}}]}`)
				return
			}
			w.Header().Set("Link", `<`+base+`/user/installations?per_page=100&page=2>; rel="next"`)
			fmt.Fprint(w, `{"installations":[{"id":11,"account":{"login":"other"}}]}`)
		})
		id, err := AssociatedByAccount(ctx, base, "ghu_test", "acme")
		if err != nil || id != 42 {
			t.Errorf("AssociatedByAccount = (%d, %v), want (42, nil)", id, err)
		}
	})

	t.Run("TransportFailureIsUndetermined", func(t *testing.T) {
		_, err := AssociatedByAccount(ctx, "http://127.0.0.1:1", "ghu_test", "acme")
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("AssociatedByAccount = %v, want ErrUndetermined", err)
		}
		if errors.Is(err, ErrNotAssociated) {
			t.Error("a transport failure must not read as a definitive no")
		}
	})

	t.Run("MalformedBodyIsUndetermined", func(t *testing.T) {
		base := fakeGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `not json`)
		})
		if _, err := AssociatedByAccount(ctx, base, "ghu_test", "acme"); !errors.Is(err, ErrUndetermined) {
			t.Errorf("AssociatedByAccount = %v, want ErrUndetermined", err)
		}
	})
}

func TestAuthority_OrganizationTarget(t *testing.T) {
	ctx := context.Background()
	target := Account{Type: "Organization", Login: "acme", ID: 7}
	actor := Actor{Login: "octocat", ID: 99}

	membership := func(t *testing.T, status int, body string) string {
		t.Helper()
		return fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			// /api/v3 because the stub is not github.com — the same GHES mount
			// every other GitHub read in the codebase derives, and proof that
			// the gate asks about the TARGET ACCOUNT rather than reading the
			// caller's own membership list.
			if want := "/api/v3/orgs/acme/memberships/octocat"; r.URL.Path != want {
				t.Errorf("path = %q, want %q", r.URL.Path, want)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		})
	}

	t.Run("ActiveAdminPasses", func(t *testing.T) {
		base := membership(t, http.StatusOK, `{"state":"active","role":"admin"}`)
		if err := Authority(ctx, base, "ghu_test", target, actor); err != nil {
			t.Errorf("Authority = %v, want nil", err)
		}
	})

	// The contractor case, and the reason this ticket is as big as it is: a
	// user who passes the association gate (they have :read on one repo inside
	// the installation) and is only a member of the account must be refused.
	t.Run("MemberIsRefused_TheContractorCase", func(t *testing.T) {
		base := membership(t, http.StatusOK, `{"state":"active","role":"member"}`)
		if err := Authority(ctx, base, "ghu_test", target, actor); !errors.Is(err, ErrNotAdmin) {
			t.Errorf("Authority = %v, want ErrNotAdmin", err)
		}
	})

	// billing_manager is a real value of GitHub's role enum and reads like an
	// administrative role. It is not one.
	t.Run("BillingManagerIsRefused", func(t *testing.T) {
		base := membership(t, http.StatusOK, `{"state":"active","role":"billing_manager"}`)
		if err := Authority(ctx, base, "ghu_test", target, actor); !errors.Is(err, ErrNotAdmin) {
			t.Errorf("Authority = %v, want ErrNotAdmin", err)
		}
	})

	t.Run("PendingAdminIsRefused", func(t *testing.T) {
		// An invited-but-not-accepted admin does not administer the account
		// today.
		base := membership(t, http.StatusOK, `{"state":"pending","role":"admin"}`)
		if err := Authority(ctx, base, "ghu_test", target, actor); !errors.Is(err, ErrNotAdmin) {
			t.Errorf("Authority = %v, want ErrNotAdmin", err)
		}
	})

	t.Run("MissingStateIsUndetermined", func(t *testing.T) {
		// An absent state is not evidence that the caller is not an admin. It
		// fails safe either way, but the copy differs — "try again" rather than
		// "you're not an admin of acme" — so the classification has to be right.
		base := membership(t, http.StatusOK, `{"role":"admin"}`)
		err := Authority(ctx, base, "ghu_test", target, actor)
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
		if errors.Is(err, ErrNotAdmin) {
			t.Error("a missing membership state must not read as a definitive no")
		}
	})

	t.Run("UnknownStateIsUndetermined", func(t *testing.T) {
		base := membership(t, http.StatusOK, `{"state":"suspended","role":"admin"}`)
		if err := Authority(ctx, base, "ghu_test", target, actor); !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
	})

	t.Run("UnknownRoleIsUndetermined", func(t *testing.T) {
		// A role this build cannot rank is not a verdict it reached.
		base := membership(t, http.StatusOK, `{"state":"active","role":"maintainer"}`)
		err := Authority(ctx, base, "ghu_test", target, actor)
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
		if errors.Is(err, ErrNotAdmin) {
			t.Error("an unrankable role must not read as a definitive no")
		}
	})

	t.Run("ForbiddenIsUndetermined", func(t *testing.T) {
		// The 403 a lost `members: read` produces. It says nothing about the
		// user, so it must not be reported as a fact about them — and it still
		// refuses.
		base := membership(t, http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)
		err := Authority(ctx, base, "ghu_test", target, actor)
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
		if err == nil {
			t.Fatal("a 403 must never pass the authority gate")
		}
	})

	t.Run("NotFoundIsUndetermined", func(t *testing.T) {
		base := membership(t, http.StatusNotFound, `{"message":"Not Found"}`)
		if err := Authority(ctx, base, "ghu_test", target, actor); !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
	})

	t.Run("TransportFailureIsUndetermined", func(t *testing.T) {
		err := Authority(ctx, "http://127.0.0.1:1", "ghu_test", target, actor)
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
	})

	t.Run("MalformedBodyIsUndetermined", func(t *testing.T) {
		base := membership(t, http.StatusOK, `not json`)
		if err := Authority(ctx, base, "ghu_test", target, actor); !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
	})
}

func TestAuthority_UserTarget(t *testing.T) {
	ctx := context.Background()
	// A user target asks GitHub nothing, so any base URL would do; a handler
	// that fails the test is what proves it.
	base := fakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a user-target authority check called GitHub at %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	t.Run("SameAccountPasses", func(t *testing.T) {
		err := Authority(ctx, base, "ghu_test",
			Account{Type: "User", Login: "octocat", ID: 99}, Actor{Login: "octocat", ID: 99})
		if err != nil {
			t.Errorf("Authority = %v, want nil", err)
		}
	})

	t.Run("AnotherAccountIsRefused", func(t *testing.T) {
		// Installed on somebody else's personal account. Nobody administers
		// another person's account, so there is no arm that could pass.
		err := Authority(ctx, base, "ghu_test",
			Account{Type: "User", Login: "victim", ID: 7}, Actor{Login: "octocat", ID: 99})
		if !errors.Is(err, ErrNotAdmin) {
			t.Errorf("Authority = %v, want ErrNotAdmin", err)
		}
	})

	t.Run("RenamedLoginDoesNotDecideIt", func(t *testing.T) {
		// Matching logins on different numeric ids must not pass: a login is
		// renameable, and a comparison of renameable strings is one that can be
		// arranged.
		err := Authority(ctx, base, "ghu_test",
			Account{Type: "User", Login: "octocat", ID: 7}, Actor{Login: "octocat", ID: 99})
		if !errors.Is(err, ErrNotAdmin) {
			t.Errorf("Authority = %v, want ErrNotAdmin", err)
		}
	})

	t.Run("MissingIDIsUndetermined", func(t *testing.T) {
		err := Authority(ctx, base, "ghu_test",
			Account{Type: "User", Login: "octocat"}, Actor{Login: "octocat", ID: 99})
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
	})

	t.Run("UnknownTargetTypeIsUndetermined", func(t *testing.T) {
		err := Authority(ctx, base, "ghu_test",
			Account{Type: "Enterprise", Login: "acme", ID: 7}, Actor{Login: "octocat", ID: 99})
		if !errors.Is(err, ErrUndetermined) {
			t.Errorf("Authority = %v, want ErrUndetermined", err)
		}
	})
}
