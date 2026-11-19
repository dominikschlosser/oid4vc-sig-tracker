"""The texts the post service in api/ posts to GitHub, as build.py exports them."""
import re

# How the review answers a comment by a person. See review-rules.md.
STATUS = {"open": "Still open", "fixed": "Fixed", "disagree": "Disagree", "answered": "Answer"}


def answered_to(d):
    """Who raised the point and where, such as "Awambeng on EtsiTrustListIdentityProvider.java:42"."""
    who = ", ".join(d["authors"])
    return f'{who} on {d["where"].rsplit("/", 1)[-1]}' if d["where"] else f'{who} in {"a review" if d["kind"] == "review" else "a comment"}'


# The parts of model text that quiet() keeps as they are, and the bare @ that it changes. They follow Markdown.
# A fence starts and ends a line. It may be indented, or follow the marker of a list item or a quote (CONTAINER).
# A line that starts with ``` and has another backtick is inline code, not a fence, and a closing fence has nothing
# after it. A fence that isn't closed runs to the end of its container: to the end of the text, to a blank line
# in a quote, or in a list item to a blank line followed by a line that isn't indented. review.shorten() follows
# the same rules.
# Each repetition takes one space or marker, so a long indentation can't be split in many ways.
CONTAINER = r"(?P<container>(?:[ \t]|(?:[-*+]|\d+[.)])[ \t]|(?P<quote>>))+)?"
FENCE = (r"(?m:^)" + CONTAINER + r"(?P<fence>```+(?![^\n]*`)|~~~+)[\s\S]*?"
         r"(?:\n[ \t]*(?:>[ \t]?)*(?P=fence)(?:`*|~*)[ \t]*(?=\n|\Z)|(?(quote)(?=\n[ \t]*\n)|(?(container)(?=\n[ \t]*\n(?![ \t]))|(?!)))|\Z)")
# A code span ends where a new block starts, such as a blank line, a list item, a quote, a heading or a fence,
# so a lone backtick doesn't hide the mentions after it. It closes only with a run of backticks as long as the
# one it opened with.
SPAN = r"(?<!`)(?P<ticks>`+)(?!`)(?:(?!\n[ \t]*(?:\n|[-*+>#]|\d+[.)][ \t]|```|~~~))[\s\S])*?(?<!`)(?P=ticks)(?!`)"
# A link target only counts after its [text] and has no spaces. The text has no brackets, so a lone [ earlier on
# doesn't reach to a later link.
LINK = r"\[[^\[\]]*\]\([^)\s]*\)|https?://\S+"
# An underscore before the @ doesn't count as part of a word, since _@bob_ renders as emphasis around a mention.
MENTION = r"(?<![A-Za-z0-9])@(?=[A-Za-z0-9])"
QUIET = re.compile("|".join((FENCE, SPAN, LINK, MENTION)))


def quiet(s):
    """Model text for GitHub without @-mentions, which would notify people. Markdown code and links stay as they
    are, and elsewhere a zero-width space after the @ keeps GitHub from linking the login. HTML code, in <code>
    or <pre>, isn't protected, since the models write Markdown. Indented code blocks aren't told apart from
    indented list items, so an @ in them gets the space too."""
    return QUIET.sub(lambda m: "@\u200b" if m.group(0) == "@" else m.group(0), s)


def postable(d):
    """Whether the answer can be posted. Thread answers from older reviews have no last field. Without it the
    post service can't tell whether a newer reply overtook them."""
    return d["kind"] != "thread" or bool(d.get("last"))


def reply_export(d):
    """An answer as the post service sends it. Its id is the ref, which stays the same when the review is redone.
    Threads get a reply. Comments and reviews can't have replies on GitHub, so the answer goes into a new
    comment on the pull request that links the original."""
    # The label is its own paragraph, so an answer that starts with a code block, a list or a quote stays one.
    said = quiet(f'**{STATUS[d["status"]]}:**\n\n{d["answer"]}')
    if d["kind"] == "thread":
        # last is the newest comment in the thread when the review was written.
        return {"id": d["ref"], "thread": int(d["ref"][1:]), "last": d["last"], "label": answered_to(d), "body": said}
    link = re.sub(r"([\[\]])", r"\\\1", answered_to(d))
    # The point is quoted, since it says what the other person wrote, not what the tracker thinks.
    point = "\n".join("> " + line for line in quiet(d["point"]).split("\n"))
    return {"id": d["ref"], "thread": 0, "label": answered_to(d), "body": f'Re [{link}]({d["url"]}):\n\n{point}\n\n{said}'}
