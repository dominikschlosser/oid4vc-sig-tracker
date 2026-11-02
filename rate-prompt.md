Rate each item in unrated.json in the current directory. These are new open Keycloak OpenID4VC issues and PRs. Rate how much each one matters for using Keycloak as an issuer or verifier in a production EUDI Wallet ecosystem. Judge each item on its own merit and ignore who wrote it.

Read the full item with `gh issue view N -R keycloak/keycloak --comments` or `gh pr view N -R keycloak/keycloak --comments` when the excerpt isn't enough, especially for security claims and items that may already be fixed.

Calibrate against ratings.json, which holds the reviewed ratings of the other open items. A new item should fit that scale.

Spec quotes you can cite are in specs.json (id, doc, ref, text). Cite at most one id, the rule that defines the requirement. Prefer the ARF over ETSI. Use only ids from that file.

Scale:
- blocker: without it, Keycloak can't run lawfully or practically in a production EUDI deployment. Workarounds don't count if they don't scale, such as registering every wallet as a client, configuring trust anchors by hand or reissuing credentials every day. This includes security holes that let someone get or use credentials of other users.
- high: required by an implementing regulation or HAIP, needed by most deployments, or a security issue that needs more than one step to exploit.
- medium: improves EUDI flows or is needed by some deployments.
- low (shown as Normal): needed work that EUDI flows don't wait for, such as admin UI, docs, tests, refactoring and cleanup. Also items that look already fixed.
- none: unrelated to EUDI use. These items are left off the page.

Basis: legal (a CIR, or an ARF or ETSI requirement it relies on), profile (HAIP or the OpenID4VC specs), usable (no rule demands it, but real deployments can't do without it), security, hygiene.

Context: the German PID has no stable identifier, so relying parties often need the PID plus a credential of their own, and issuers need to bind a new credential to a PID. Verifier revocation checks are only a SHOULD in ARF VCR_13. No spec requires CRL or OCSP on issuer chains.

For each item you rate blocker or high, name to yourself the concrete production deployment that fails without it. Downgrade if you can't name one.

Write new-ratings.json as an object keyed by item number: {"rating": ..., "basis": ..., "specs": [one id or none], "note": "...", "why": "..."}. For blocker and high items, "why" explains the level in one or two sentences: what fails without it, and for high why it is not a blocker. Leave "why" empty for other items. The note is one or two short plain sentences ending with a period, without semicolons or dashes, like the notes in ratings.json. Name the subject. Avoid comma splices and fragments. Include every item from unrated.json.
