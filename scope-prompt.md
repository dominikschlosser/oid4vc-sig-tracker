scope-input.json in the current directory lists new issues and PRs of keycloak/keycloak. None of them names OpenID4VC, verifiable credentials or the other search words of the tracker. Pick the ones that still matter for using Keycloak as an issuer or verifier in a production EUDI Wallet ecosystem.

They fit when they change something an EUDI issuer or verifier relies on. Examples are JOSE and JWE algorithms, X.509 trust chains and certificate revocation, trusted lists, DPoP, PAR, client attestations, the Digital Credentials API, token and key handling in the flows that issue or verify credentials, and security holes in them. They don't fit when they only concern other Keycloak features, such as LDAP, SAML, themes, the admin console in general, upgrades or build tooling.

Read the full item with `gh issue view N -R keycloak/keycloak` or `gh pr view N -R keycloak/keycloak` when the excerpt isn't enough. When in doubt, pick the item. The rating step that follows can still rate it as unrelated.

Write scope-answer.json as {"in": [numbers of the items that fit]}. Write {"in": []} when none fits.
