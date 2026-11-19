"""Tests for which comments the review answers and what starts a new review. Run with python3 -m unittest test_review."""
import re
import unittest

import fetch

from posts import postable, quiet, reply_export
import review
from review import LEFT_OUT, Comment, Entry, answered, clean, discussed, is_current, reviewed_by, shorten

SIGN = "\n\n<sub>Automatic review by Claude Opus 5.5: https://tracker.example/reviews/7.html{}</sub>"


def person(name, body="Why?", id=0):
    return Comment(name, False, "2026-11-18", f"https://github.com/c{id}", body, id)


def bot(name, body="Looks off.", id=0):
    return Comment(name, True, "2026-11-18", f"https://github.com/c{id}", body, id)


def tracker(anchor="", id=0):
    return Comment("tracker-bot", False, "2026-11-18", f"https://github.com/c{id}", "**Fixed:** in A.java" + SIGN.format(anchor), id)


def thread(*comments):
    return Entry(f"t{comments[0].id}", "thread", list(comments))


def names(cs):
    return [c.author for c in cs]


class RaisedBy(unittest.TestCase):
    def test_a_person_s_thread_counts_without_the_pr_author_s_replies(self):
        t = thread(person("bob", id=1), person("author", id=2), person("carol", id=3))
        self.assertEqual(names(t.raised_by("author")), ["bob", "carol"])

    def test_the_pr_author_s_own_thread_counts_whole(self):
        t = thread(person("author", id=1), person("author", id=2))
        self.assertEqual(names(t.raised_by("author")), ["author", "author"])

    def test_only_replies_after_the_tracker_s_last_post_count(self):
        t = thread(person("bob", id=1), tracker(id=2), person("author", "Not true.", id=3))
        self.assertEqual(names(t.raised_by("author")), ["author"])

    def test_a_thread_the_tracker_answered_last_needs_no_answer(self):
        t = thread(person("bob", id=1), person("author", id=2), tracker(id=3))
        self.assertEqual(t.raised_by("author"), [])

    def test_a_bot_s_reply_to_the_tracker_counts(self):
        t = thread(tracker("#c0", id=1), bot("copilot", id=2), person("author", id=3))
        self.assertEqual(names(t.raised_by("author")), ["copilot", "author"])

    def test_a_bot_s_thread_without_the_tracker_needs_no_answer(self):
        t = thread(bot("copilot", id=1), person("author", "Done.", id=2))
        self.assertEqual(t.raised_by("author"), [])

    def test_a_long_thread_without_a_loaded_tracker_post_counts_only_its_newest_replies(self):
        t = thread(person("bob", id=1), person("carol", id=60), person("dave", id=61))
        t.flags = [LEFT_OUT]
        self.assertEqual(names(t.raised_by("author")), ["carol", "dave"])

    def test_a_comment_counts_until_the_tracker_answered_it(self):
        c = Entry("c9", "comment", [person("bob", id=9)])
        self.assertEqual(names(c.raised_by("author")), ["bob"])
        self.assertEqual(c.raised_by("author", {"c9"}), [])

    def test_bot_comments_on_the_pull_request_need_no_answer(self):
        self.assertEqual(Entry("c9", "comment", [bot("keycloak-github-bot", id=9)]).raised_by("author"), [])


class TrackerPosts(unittest.TestCase):
    def test_a_quoted_tracker_post_is_a_person_s_comment(self):
        quoted = "> **Fixed:** in A.java\n>\n> " + SIGN.format("#answer-t1").strip() + "\n\nNo, it isn't."
        self.assertFalse(person("bob", quoted).tracker)
        self.assertTrue(tracker("#answer-t1").tracker)

    def test_a_post_that_ends_with_a_quote_of_a_tracker_post_is_a_person_s_comment(self):
        quoted = "No, it isn't:\n\n> **Fixed:** in A.java\n>\n> " + SIGN.format("#answer-t1").strip() + "\n"
        self.assertFalse(person("bob", quoted).tracker)

    def test_a_hidden_tracker_post_still_counts_as_answered(self):
        t = thread(person("bob", id=1), tracker(id=2), person("author", "Not true.", id=3))
        t.comments[1].hidden = True
        self.assertEqual(names(t.raised_by("author")), ["author"])
        c = tracker("#answer-c9", id=4)
        c.hidden = True
        self.assertEqual(answered([Entry("c4", "comment", [c])]), {"c9"})

    def test_a_hidden_comment_needs_no_answer(self):
        c = person("bob", id=9)
        c.hidden = True
        self.assertEqual(Entry("c9", "comment", [c]).raised_by("author"), [])

    def test_answered_finds_the_refs_of_tracker_comments_that_link_an_answer(self):
        entries = [Entry("c1", "comment", [tracker("#answer-c9", id=1)]), Entry("c2", "comment", [tracker("#answer-r4", id=2)]),
                   Entry("c3", "comment", [person("bob", "see #answer-c5</sub>", id=3)])]
        self.assertEqual(answered(entries), {"c9", "r4"})


def review_node(at, kind, body="", first=""):
    return {"submittedAt": at, "body": body, "author": {"__typename": kind}, "comments": {"nodes": [{"body": first}]}}


def thread_node(*comments, hidden=()):
    return {"comments": {"nodes": [{"createdAt": at, "body": body, "isMinimized": at in hidden, "author": {"__typename": kind}}
                                   for at, kind, body in comments]}}


class Discussed(unittest.TestCase):
    def test_the_tracker_s_thread_replies_start_no_review(self):
        pr = {"lastComments": {"nodes": []}, "lastReviews": {"nodes": [
            review_node("2026-11-01", "User", first="Why?"), review_node("2026-11-02", "User", first="**Fixed:**" + SIGN.format("#answer-t1"))]}}
        self.assertEqual(discussed(pr), "2026-11-01")

    def test_a_bot_s_reply_after_the_tracker_s_post_starts_a_review(self):
        pr = {"lastComments": {"nodes": []}, "lastReviews": {"nodes": [review_node("2026-11-04", "Bot", body="## Pull request overview")]},
              "lastThreads": {"nodes": [
                  thread_node(("2026-11-01", "User", "Why?"), ("2026-11-02", "User", "x" + SIGN.format("#answer-t1")), ("2026-11-03", "Bot", "Disagree.")),
                  thread_node(("2026-11-01", "Bot", "Looks off."), ("2026-11-05", "Bot", "Still off."))]}}
        self.assertEqual(discussed(pr), "2026-11-03")

    def test_a_person_s_thread_reply_counts_after_many_tracker_replies(self):
        tracker_replies = [review_node(f"2026-11-1{i}", "User", first="x" + SIGN.format(f"#answer-t{i}")) for i in range(10)]
        pr = {"lastComments": {"nodes": []}, "lastReviews": {"nodes": tracker_replies},
              "lastThreads": {"nodes": [thread_node(("2026-11-02", "User", "x" + SIGN.format("#answer-t1")), ("2026-11-03", "User", "No."))]}}
        self.assertEqual(discussed(pr), "2026-11-03")

    def test_a_hidden_bot_reply_starts_no_review(self):
        pr = {"lastComments": {"nodes": []}, "lastReviews": {"nodes": []},
              "lastThreads": {"nodes": [thread_node(("2026-11-02", "User", "x" + SIGN.format("#answer-t1")), ("2026-11-03", "Bot", "Spam."),
                                                    hidden=("2026-11-03",))]}}
        self.assertEqual(discussed(pr), "")

    def test_a_draft_reply_in_a_pending_review_starts_no_review(self):
        threads = thread_node(("2026-11-01", "User", "Why?"), ("2026-11-03", "User", "Draft."))
        threads["comments"]["nodes"][1]["state"] = "PENDING"
        pr = {"lastComments": {"nodes": []}, "lastReviews": {"nodes": []}, "lastThreads": {"nodes": [threads]}}
        self.assertEqual(discussed(pr), "2026-11-01")

    def test_a_tracker_post_by_a_bot_account_starts_no_review(self):
        pr = {"lastComments": {"nodes": []}, "lastReviews": {"nodes": []},
              "lastThreads": {"nodes": [thread_node(("2026-11-01", "User", "Why?"), ("2026-11-02", "Bot", "x" + SIGN.format("#answer-t1")),
                                                    ("2026-11-03", "Bot", "y" + SIGN.format("#answer-t1")))]}}
        self.assertEqual(discussed(pr), "2026-11-01")

    def test_a_thread_reply_counts_from_when_its_review_went_out(self):
        # GitHub creates the comment a second or more after the review that it belongs to.
        threads = thread_node(("2026-10-15T14:48:07Z", "User", "Why?"))
        threads["comments"]["nodes"][0]["pullRequestReview"] = {"submittedAt": "2026-10-15T14:48:06Z"}
        pr = {"lastComments": {"nodes": []}, "lastReviews": {"nodes": [review_node("2026-10-15T14:48:06Z", "User", first="Why?")]},
              "lastThreads": {"nodes": [threads]}}
        self.assertEqual(discussed(pr), "2026-10-15T14:48:06Z")

    def test_people_s_comments_count_and_tracker_comments_don_t(self):
        pr = {"lastComments": {"nodes": [{"createdAt": "2026-11-05", "body": "Hm", "author": {"__typename": "User"}},
                                         {"createdAt": "2026-11-06", "body": "x" + SIGN.format("#answer-c1"), "author": {"__typename": "User"}}]},
              "lastReviews": {"nodes": []}}
        self.assertEqual(discussed(pr), "2026-11-05")


class Conversation(unittest.TestCase):
    def test_drafts_in_a_pending_review_stay_out(self):
        def node(i, body, state, date="createdAt"):
            return {"body": body, "author": {"login": "dominik", "__typename": "User"}, "databaseId": i, "url": f"u{i}",
                    "isMinimized": False, "state": state, date: "2026-11-0%d" % i}
        thread = {"isResolved": False, "isOutdated": False, "path": "A.java", "line": 3, "originalLine": 3,
                  "first": {"nodes": [node(1, "Why?", "SUBMITTED")]},
                  "last": {"totalCount": 2, "nodes": [node(1, "Why?", "SUBMITTED"), node(2, "Draft.", "PENDING")]}}
        pr = {"comments": {"totalCount": 0, "nodes": []},
              "reviews": {"totalCount": 2, "nodes": [node(3, "Fine.", "COMMENTED", "submittedAt"), node(4, "Draft.", "PENDING", "submittedAt")]},
              "reviewThreads": {"totalCount": 1, "nodes": [thread]}}
        gh, review.gh = review.gh, lambda *a: {"data": {"repository": {"pullRequest": pr}}}
        try:
            _, entries, _ = review.conversation(7)
        finally:
            review.gh = gh
        self.assertEqual([(e.ref, [c.body for c in e.comments], e.flags) for e in entries], [("t1", ["Why?"], []), ("r3", ["Fine."], [])])


class Threads(unittest.TestCase):
    def test_threads_load_ten_pull_requests_per_query(self):
        queries = []
        def gh(*args):
            q = args[-1]
            queries.append(q)
            nums = [int(n) for n in re.findall(r"n(\d+):pullRequest", q)]
            return {"data": {"repository": {f"n{n}": {"number": n, "lastThreads": {"nodes": [n]}} for n in nums}}}
        old, fetch.gh = fetch.gh, gh
        try:
            found = fetch.threads(range(12, 0, -1))
        finally:
            fetch.gh = old
        self.assertEqual(len(queries), 2)
        self.assertEqual(found, {n: {"nodes": [n]} for n in range(1, 13)})


class Shorten(unittest.TestCase):
    def test_a_cut_closes_the_code_block_it_leaves_open(self):
        text = "Use this:\n\n~~~~java\n```\nint a = 1;\nint b = 2;\n"
        self.assertEqual(shorten(text, 40), text[:40] + "\n~~~~")
        self.assertEqual(shorten("```\na\n```\nmore text", 12), "```\na\n```\nmo")
        self.assertEqual(shorten("short", 40), "short")
        self.assertEqual(shorten("See:\n```java\na();", 40), "See:\n```java\na();\n```")

    def test_the_closing_fence_stays_in_the_list_item(self):
        self.assertEqual(shorten("- item\n  ```java\n  int x = 1;\n  int y = 2;\n", 30), "- item\n  ```java\n  int x = 1;\n  ```")

    def test_inline_code_and_a_fence_with_text_after_it_open_and_close_nothing(self):
        self.assertEqual(shorten("```x``` and code\nfoo bar baz", 20), "```x``` and code\nfoo")
        self.assertEqual(shorten("```\na\n```java\nb\nmore", 16), "```\na\n```java\nb\n```")

    def test_a_block_in_a_quote_or_list_item_ends_with_it(self):
        self.assertEqual(shorten("> ```java\n> a\n> b\nmore", 14), "> ```java\n> a\n> ```")
        self.assertEqual(shorten("> ```java\n> a\n\nmore text here", 22), "> ```java\n> a\n\nmore te")
        self.assertEqual(shorten("- ```java\n  a\n\nmore text here", 22), "- ```java\n  a\n\nmore te")
        self.assertEqual(shorten("- ```java\n  a\n\n  more text here", 22), "- ```java\n  a\n\n  more \n  ```")

    def test_a_fence_after_a_list_or_quote_marker_gets_closed(self):
        text = "- ```java\n  @Override\n  void f() {}\n  more code here"
        self.assertEqual(shorten(text, 30), text[:30] + "\n  ```")
        text = "> ```java\n> @Override\n> void f() {}\n> more"
        self.assertEqual(shorten(text, 30), text[:30] + "\n> ```")
        self.assertEqual(shorten("> ```\n> a\n> ```\nmore text", 20), "> ```\n> a\n> ```\nmore")


class ReviewedBy(unittest.TestCase):
    def test_the_tracker_s_posts_are_no_review_by_a_person(self):
        def node(login, body="", first="", kind="User"):
            return {"state": "COMMENTED", "body": body, "author": {"__typename": kind, "login": login}, "comments": {"nodes": [{"body": first}]}}
        reviews = [node("bob", "Looks fine."), node("tracker-bot", "Summary" + SIGN.format("")), node("carol", first="x" + SIGN.format("#answer-t1")),
                   node("dave", first="Why?"), node("author", "Self."), node("copilot-pull-request-reviewer", "Overview", kind="Bot")]
        self.assertEqual(reviewed_by(reviews, "author"), [["bob", "commented"], ["dave", "commented"]])

    def test_an_approval_wins_over_a_later_tracker_post_or_comment(self):
        def node(login, state, body):
            return {"state": state, "body": body, "author": {"__typename": "User", "login": login}, "comments": {"nodes": []}}
        reviews = [node("carol", "COMMENTED", "Summary" + SIGN.format("")), node("dave", "COMMENTED", "Nit.")]
        opinions = [node("carol", "APPROVED", ""), node("dave", "CHANGES_REQUESTED", "")]
        self.assertEqual(reviewed_by(reviews, "author", opinions), [["dave", "changes_requested"], ["carol", "approved"]])

    def test_a_comment_before_a_tracker_post_in_the_own_name_still_counts(self):
        def node(login, body):
            return {"state": "COMMENTED", "body": body, "author": {"__typename": "User", "login": login}, "comments": {"nodes": []}}
        recent = [node("carol", "Why this?"), node("carol", "Summary" + SIGN.format(""))]
        self.assertEqual(reviewed_by([recent[1]], "author", (), recent), [["carol", "commented"]])

    def test_a_pending_review_or_a_node_without_a_state_is_no_review(self):
        def node(login, state):
            return {"state": state, "body": "Hm.", "author": {"__typename": "User", "login": login}, "comments": {"nodes": []}}
        recent = [node("carol", "PENDING"), node("dave", None), node("erin", "COMMENTED")]
        self.assertEqual(reviewed_by([], "author", (), recent), [["erin", "commented"]])


class Current(unittest.TestCase):
    def pr(self, at):
        return {"headRefOid": "abc", "lastComments": {"nodes": [{"createdAt": at, "body": "Hm", "author": {"__typename": "User"}}]},
                "lastReviews": {"nodes": []}}

    def test_a_newer_comment_needs_a_new_review(self):
        self.assertFalse(is_current({"sha": "abc", "discussed": "2026-11-05"}, self.pr("2026-11-06"), []))

    def test_a_comment_that_dropped_out_of_view_needs_no_new_review(self):
        self.assertTrue(is_current({"sha": "abc", "discussed": "2026-11-05"}, self.pr("2026-11-04"), []))

    def test_a_new_commit_or_note_needs_a_new_review(self):
        self.assertFalse(is_current({"sha": "old", "discussed": "2026-11-05"}, self.pr("2026-11-05"), []))
        self.assertFalse(is_current({"sha": "abc", "discussed": "2026-11-05"}, self.pr("2026-11-05"), ["note"]))


class Answers(unittest.TestCase):
    def test_a_thread_answer_remembers_the_newest_comment_the_review_saw(self):
        e = thread(person("bob", id=1), person("author", "Done.", id=2))
        e.where = "src/A.java:3"
        out = clean({"verdict": "comment", "summary": "s", "discussion": [{"ref": "t1", "status": "fixed", "point": "p", "answer": "a"}]},
                    {"t1": (e, e.raised_by("author"))})
        d = out["discussion"][0]
        self.assertEqual((d["last"], d["authors"], d["url"]), (2, ["bob"], "https://github.com/c1"))
        self.assertEqual(reply_export(d), {"id": "t1", "thread": 1, "last": 2, "label": "bob on A.java:3", "body": "**Fixed:**\n\na"})

    def test_an_answer_to_a_comment_links_it_without_mentioning_anyone(self):
        d = {"ref": "c9", "kind": "comment", "status": "answered", "authors": ["bob"], "where": "", "url": "https://github.com/c9",
             "point": "Ask @carol", "answer": "Yes, see `@Inject` and https://x.dev/@a."}
        self.assertEqual(reply_export(d)["body"],
                         "Re [bob in a comment](https://github.com/c9):\n\n> Ask @\u200bcarol\n\n**Answer:**\n\nYes, see `@Inject` and https://x.dev/@a.")

    def test_a_lone_backtick_does_not_hide_mentions_in_later_paragraphs(self):
        self.assertEqual(quiet("It's `x.\n\nAsk @bob"), "It's `x.\n\nAsk @\u200bbob")

    def test_old_thread_answers_cannot_be_posted(self):
        self.assertFalse(postable({"kind": "thread", "ref": "t1"}))
        self.assertTrue(postable({"kind": "thread", "ref": "t1", "last": 3}))
        self.assertTrue(postable({"kind": "comment", "ref": "c1"}))
        # An older review's thread answer goes by the date of the newest comment the review saw.
        old = {"kind": "thread", "ref": "t1", "status": "open", "answer": "a", "authors": ["bob"], "where": "A.java:3"}
        self.assertTrue(postable(old, "2026-11-05T10:31:02Z"))
        self.assertEqual(reply_export(old, "2026-11-05T10:31:02Z"),
                         {"id": "t1", "thread": 1, "last": 0, "label": "bob on A.java:3", "body": "**Still open:**\n\na", "seen": "2026-11-05T10:31:02Z"})

    def test_a_mention_in_underscore_emphasis_gets_the_space(self):
        self.assertEqual(quiet("_@bob_ and mail@x.dev"), "_@\u200bbob_ and mail@x.dev")

    def test_a_mention_in_what_only_looks_like_code_or_a_link_gets_the_space(self):
        self.assertEqual(quiet("[note](see @bob here)"), "[note](see @\u200bbob here)")
        self.assertEqual(quiet("` @bob ``"), "` @\u200bbob ``")
        self.assertEqual(quiet("`` a ` @bob ``"), "`` a ` @bob ``")

    def test_code_in_a_fence_inside_a_list_item_or_an_unclosed_fence_stays_as_it_is(self):
        nested = "- Use this:\n\n    ```java\n    @Override\n    void a() {}\n\n    @Inject\n    Foo f;\n    ```\n\nThanks @bob"
        self.assertEqual(quiet(nested), nested.replace("@bob", "@\u200bbob"))
        self.assertEqual(quiet("x\n```\n@a\n"), "x\n```\n@a\n")

    def test_a_fence_line_with_text_after_it_doesn_t_close_the_block(self):
        text = "```\n@a\n```java\n@b\n```\nThanks @bob"
        self.assertEqual(quiet(text), text.replace("@bob", "@\u200bbob"))

    def test_an_unclosed_block_in_a_quote_or_list_item_ends_with_it(self):
        self.assertEqual(quiet("> ```\n> code @A\n\nThanks @bob"), "> ```\n> code @A\n\nThanks @\u200bbob")
        self.assertEqual(quiet("- ```\n  @A\n\nThanks @bob"), "- ```\n  @A\n\nThanks @\u200bbob")
        self.assertEqual(quiet("- ```\n  @A\n\n  @B\n  ```\n@c"), "- ```\n  @A\n\n  @B\n  ```\n@\u200bc")
        self.assertEqual(quiet("```\n@A\n\n@B"), "```\n@A\n\n@B")

    def test_an_answer_that_starts_with_a_code_block_keeps_it(self):
        d = {"kind": "thread", "ref": "t1", "last": 2, "authors": ["bob"], "where": "A.java:3", "status": "fixed", "answer": "```java\n@Override\n```"}
        self.assertEqual(reply_export(d)["body"], "**Fixed:**\n\n```java\n@Override\n```")

    def test_code_after_a_list_or_quote_marker_stays_as_it_is(self):
        for text in ("- ```java\n  @Override\n  void f();\n  ```", "> ```java\n> @Override\n> ```", "1. ```\n   @Bean\n   ```"):
            self.assertEqual(quiet(text), text)
        self.assertEqual(quiet("> ```\n> @A\n> ```\n> thanks @bob"), "> ```\n> @A\n> ```\n> thanks @\u200bbob")

    def test_a_lone_backtick_doesn_t_hide_mentions_in_the_next_block(self):
        for text in ("Call `foo()\n- @bob said `x`", "Use `a\n```\ncode\n```\nthanks @bob `x`", "See `a\n> @bob `x`"):
            self.assertEqual(quiet(text), text.replace("@bob", "@\u200bbob"))

    def test_a_line_that_starts_with_inline_triple_backticks_is_no_fence(self):
        self.assertEqual(quiet("```@Inject``` is used.\n\nThanks @bob"), "```@Inject``` is used.\n\nThanks @\u200bbob")

    def test_a_lone_bracket_doesn_t_hide_mentions_up_to_a_later_link(self):
        self.assertEqual(quiet("range [0, n) breaks for @bob, see [docs](https://x)"),
                         "range [0, n) breaks for @​bob, see [docs](https://x)")

    def test_a_deeply_indented_line_doesn_t_make_quiet_or_shorten_slow(self):
        import time
        # The old pattern took about 2 seconds on each of these, and 4 times as long for each further two characters.
        for text in (" " * 24 + "@bob", "> " * 22 + "@bob", "\t " * 12 + "@bob"):
            start = time.monotonic()
            self.assertIn("@​bob", quiet(text))
            shorten(text, 20)
            self.assertLess(time.monotonic() - start, 0.5, repr(text[:10]))

    def test_a_mention_right_after_a_full_stop_gets_the_space(self):
        self.assertEqual(quiet("Thanks.@bob and first.last@x.dev"), "Thanks.@\u200bbob and first.last@x.dev")

    def test_text_that_only_looks_like_code_or_a_link_gets_the_space(self):
        self.assertEqual(quiet("a ~~~@bob~~~ hi"), "a ~~~@\u200bbob~~~ hi")
        self.assertEqual(quiet("see x](@bob) ok"), "see x](@\u200bbob) ok")
        self.assertEqual(quiet("see [docs](https://x.dev/@a) ok"), "see [docs](https://x.dev/@a) ok")

    def test_code_blocks_keep_their_at_signs(self):
        for code in ("```java\n@Inject\n```", "~~~\n@Inject\n~~~"):
            self.assertEqual(quiet(code + "\nThanks @bob"), code + "\nThanks @\u200bbob")


if __name__ == "__main__":
    unittest.main()
