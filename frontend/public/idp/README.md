# Identity-provider marks

Vendor marks for the login identity on the personal settings page — the door
a person came in by, drawn at 12px beside the provider's name. Literal marks,
like GitHub's and Jira's in `src/ui/accounts`: a vendor's mark is the one
place the token layer does not reach.

All four come from the design handoff's `assets/idp/`, which took them from
[homarr-labs/dashboard-icons](https://github.com/homarr-labs/dashboard-icons)
(the Entra mark by way of selfh.st, under CC BY 4.0, per the handoff). Each
mark is its vendor's trademark, used to name that vendor and nothing else. The
`<metadata>` block the handoff's copies carried — a content-provenance
manifest, most of each file's bytes — is stripped here; the drawing is
otherwise as received.

| File            | Provider          | Note                                                   |
| --------------- | ----------------- | ------------------------------------------------------ |
| `entra.svg`     | Microsoft Entra   |                                                        |
| `okta.svg`      | Okta              | light ground                                           |
| `okta-dark.svg` | Okta              | dark ground — Okta ships a dark variant, so it follows the theme |
| `google.svg`    | Google            |                                                        |

OneLogin and Ping have no mark here yet; a login through either is named in
type and carries no mark, as does a connection whose product is not known.
