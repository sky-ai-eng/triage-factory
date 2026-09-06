# Identity-provider marks

Vendor marks for the login identity on the personal settings page — the door
a person came in by, drawn at 12px beside the provider's name. Literal marks,
like GitHub's and Jira's in `src/ui/accounts`: a vendor's mark is the one
place the token layer does not reach.

| File            | Provider        | Source                                            |
| --------------- | --------------- | ------------------------------------------------- |
| `entra.svg`     | Microsoft Entra | homarr-labs/dashboard-icons (via selfh.st, CC BY 4.0) |
| `okta.svg`      | Okta            | homarr-labs/dashboard-icons                       |
| `okta-dark.svg` | Okta, dark ground | homarr-labs/dashboard-icons — Okta ships a dark variant, so it follows the theme |
| `google.svg`    | Google          | homarr-labs/dashboard-icons                       |

OneLogin and Ping have no mark here yet; a login through either is named in
type and carries no mark, as does a connection whose product is not known.
