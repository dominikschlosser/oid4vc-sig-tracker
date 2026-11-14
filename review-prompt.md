Review Keycloak pull request #{n}. The current directory is a checkout of its head commit.

- .pr/info.md has the title, the description and the conversation so far.
- .pr/diff.patch is the diff against the merge base with {base}.
- .pr/files.txt lists every file in the repository.
- .pr/notes.md, if it exists, has design decisions the PR author confirmed to the tracker.

Read the diff, then the changed files in full and the code they call. You can't build or run anything, so verify each finding by reading the code.

Look for, in this order:
1. Bugs and security problems. Think about credential issuance and presentation, tokens, nonces, replay, key binding, signatures and trust chains, input validation, and data of one user reaching another.
2. Places where the change breaks OpenID4VCI 1.0, OpenID4VP 1.0, HAIP 1.0, SD-JWT VC, ISO 18013-5 or the EUDI ARF. Name the section.
3. Missing tests for the new behaviour, and upgrade or compatibility problems.
4. Design problems that make the code hard to keep.

{rules}
