Another reviewer wrote a draft review of Keycloak pull request #{n}. Check it against the code and write the final review. The current directory is a checkout of the head commit of the PR.

- .pr/draft.json is the draft review.
- .pr/info.md has the title, the description and the conversation so far.
- .pr/diff.patch is the diff against the merge base with {base}.
- .pr/files.txt lists every file in the repository.
- .pr/notes.md, if it exists, has design decisions the PR author confirmed to the tracker.

You can't build or run anything, so check by reading the code. Treat the draft as a claim to test, not as the truth.

- Verify every finding at its file and line. Drop findings that are wrong or that fail the production value check below. Correct the file, the line, the severity and the wording where they are off.
- Look for problems the draft missed, in the diff and in the code it calls. Add only what you verified and what passes the same check.
- Check every answer in `discussion` against the comment and the code. Add answers for refs that raise a point and have none.
- Make the verdict, the headline and the summary match the final findings.

Keep what is right as it is. Add a `changes` list to the answer format below, next to `discussion`: `"changes": ["One sentence per change you made to the draft and why."]`. Leave it empty when the draft holds.

{rules}
