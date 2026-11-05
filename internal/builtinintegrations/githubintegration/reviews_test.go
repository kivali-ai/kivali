package githubintegration

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// reviewsAnswer is list_pull_request_reviews' answer as the agent reads
// it.
type reviewsAnswer struct {
	Total   int         `json:"total"`
	Reviews []reviewOut `json:"reviews"`
	Note    string      `json:"note"`
}

// reviewCommentsAnswer is list_review_comments' answer as the agent
// reads it.
type reviewCommentsAnswer struct {
	Total    int                `json:"total"`
	Comments []reviewCommentOut `json:"comments"`
	Note     string             `json:"note"`
}

// oldestFirst answers a listing as GitHub lists reviews and inline
// comments: oldest first, the page size asked for, item(i) for each of
// total, i from 1.
func oldestFirst(total int, item func(i int) map[string]any) func(recorded) reply {
	return func(r recorded) reply {
		page, _ := strconv.Atoi(r.Query.Get("page"))
		size, _ := strconv.Atoi(r.Query.Get("per_page"))
		out := []map[string]any{}
		for i := (page-1)*size + 1; i <= min(page*size, total); i++ {
			out = append(out, item(i))
		}
		b, _ := json.Marshal(out)
		return reply{Status: 200, Body: string(b)}
	}
}

// reviewItem is GitHub's review number i, by a person, with a short
// body.
func reviewItem(i int) map[string]any {
	return map[string]any{"id": i, "user": map[string]any{"login": "reviewer"}, "state": "COMMENTED", "commit_id": headSHA,
		"submitted_at": "2026-11-03T10:00:00Z", "body": fmt.Sprintf("review %d", i)}
}

// Why this test exists: an agent asked whether a pull request is
// approved needs the latest reviews, and GitHub lists them oldest first
// and counts them nowhere: reading its first page would answer with
// stale verdicts on a long pull request. The answer must keep the end of
// the listing, in GitHub's order, say how many it left out, carry what
// decides the question (author, state, commit), and clip a long body with
// a note. Any agent whose access includes the repository may ask, with a
// token narrowed to it and to reading pull requests; a repository
// outside the access is refused before GitHub is asked anything.
func TestListPullRequestReviewsAnswersTheLatest(t *testing.T) {
	g, f := newTest(t)
	f.routeFunc("GET /repos/acme/widgets/pulls/7/reviews", oldestFirst(130, func(i int) map[string]any {
		r := reviewItem(i)
		if i == 129 {
			r["state"], r["body"] = "CHANGES_REQUESTED", strings.Repeat("é", reviewBodyClip+1)
		}
		return r
	}))
	reader := agent(false, "acme/*")

	r := g.callT(t, reader, "list_pull_request_reviews", map[string]any{"repository": "acme/widgets", "pull_request": 7, "limit": 3})
	if r.isError {
		t.Fatalf("answer = %+v", r)
	}
	var out reviewsAnswer
	if err := json.Unmarshal([]byte(r.text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 130 || len(out.Reviews) != 3 || out.Reviews[0].ID != 128 || out.Reviews[2].ID != 130 {
		t.Fatalf("out = %+v", out)
	}
	want := reviewOut{ID: 130, Author: "reviewer", State: "COMMENTED", Commit: headSHA, SubmittedAt: "2026-11-03T10:00:00Z", Body: "review 130"}
	if out.Reviews[2] != want {
		t.Errorf("latest = %+v", out.Reviews[2])
	}
	if c := out.Reviews[1]; c.State != "CHANGES_REQUESTED" || c.Body != strings.Repeat("é", reviewBodyClip) {
		t.Errorf("long one = %q, %d bytes", c.State, len(c.Body))
	}
	for _, w := range []string{"Only the latest 3 of its 130 reviews are here.", "A review's body is cut to its first 5000 characters."} {
		if !strings.Contains(out.Note, w) {
			t.Errorf("note %q lacks %q", out.Note, w)
		}
	}
	if got := mintedFor(f); len(got) != 1 || got[0] != "pull_requests=read widgets" {
		t.Errorf("mints = %v", got)
	}

	before := len(f.requests())
	if r := g.callT(t, reader, "list_pull_request_reviews", map[string]any{"repository": "other/thing", "pull_request": 7}); r.refusal == nil {
		t.Errorf("another repository = %+v", r)
	}
	if r := g.callT(t, reader, "list_pull_request_reviews", map[string]any{"repository": "acme/widgets", "pull_request": 0}); r.refusal == nil {
		t.Errorf("no pull request = %+v", r)
	}
	if len(f.requests()) != before {
		t.Error("a refused call reached GitHub")
	}
}

// Why this test exists: a review's author may have left it blank, and a
// deleted account comes back as a null user, and a pull request with no
// reviews at all is the usual case for a young one: none of these may
// fail the answer or draw a note about something left out.
func TestListPullRequestReviewsReadsEmptyAnswers(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/pulls/7/reviews", 200, `[{"id":1,"user":null,"state":"APPROVED","commit_id":"`+headSHA+`","body":null}]`)
	f.route("GET /repos/acme/widgets/pulls/8/reviews", 200, `[]`)
	reader := agent(false, "acme/*")

	var out reviewsAnswer
	r := g.callT(t, reader, "list_pull_request_reviews", map[string]any{"repository": "acme/widgets", "pull_request": 7})
	if err := json.Unmarshal([]byte(r.text), &out); err != nil || r.isError {
		t.Fatal(err, r.text)
	}
	if out.Total != 1 || out.Reviews[0].Author != "" || out.Reviews[0].State != "APPROVED" || out.Note != "" {
		t.Errorf("out = %+v", out)
	}
	if strings.Contains(r.text, "submitted_at") {
		t.Errorf("answer names a time GitHub did not: %s", r.text)
	}
	r = g.callT(t, reader, "list_pull_request_reviews", map[string]any{"repository": "acme/widgets", "pull_request": 8})
	if r.isError || r.text != `{"total":0,"reviews":[]}` {
		t.Errorf("none = %+v", r)
	}
}

// Why this test exists: GitHub gives no count of reviews, so the end of
// the listing is found by reading it, up to ten pages. Past that, what
// is at hand is not the latest, and saying "the latest" would tell an
// agent a stale verdict is the current one.
func TestListPullRequestReviewsSaysWhenItStoppedReading(t *testing.T) {
	g, f := newTest(t)
	f.routeFunc("GET /repos/acme/widgets/pulls/7/reviews", oldestFirst(1200, reviewItem))
	r := g.callT(t, agent(false, "acme/*"), "list_pull_request_reviews", map[string]any{"repository": "acme/widgets", "pull_request": 7, "limit": 1})
	var out reviewsAnswer
	if err := json.Unmarshal([]byte(r.text), &out); err != nil {
		t.Fatal(err, r.text)
	}
	if len(f.api()) != maxListPages || len(out.Reviews) != 1 || out.Reviews[0].ID != 1000 {
		t.Errorf("%d requests, out = %+v", len(f.api()), out)
	}
	if want := "GitHub lists at least 1000 reviews; these are the last of the first 1000, so later ones are not here."; out.Note != want {
		t.Errorf("note = %q", out.Note)
	}
}

// reviewCommentItem is GitHub's inline comment number i.
func reviewCommentItem(i int) map[string]any {
	return map[string]any{"id": i, "user": map[string]any{"login": "u"}, "path": "main.go", "line": 10, "original_line": 12, "side": "RIGHT",
		"commit_id": headSHA, "created_at": "2026-11-03T10:00:00Z", "html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/7#discussion_r%d", i),
		"body": fmt.Sprintf("comment %d", i), "diff_hunk": "@@ -1 +1 @@\n-a\n+b"}
}

// Why this test exists: GitHub lists a pull request's inline comments
// by id ascending, so a first page answers with the oldest, and an agent
// working through review feedback would act on what was said first and
// miss what was said last. The answer must end at the latest comment,
// read across the page boundary when the last page holds too few, stay
// oldest first as a conversation reads, and say that it left some out.
func TestListReviewCommentsAnswersTheLatestInOrder(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/pulls/7", 200, `{"number":7,"review_comments":105}`)
	f.routeFunc("GET /repos/acme/widgets/pulls/7/comments", oldestFirst(105, reviewCommentItem))

	r := g.callT(t, agent(false, "acme/*"), "list_review_comments", map[string]any{"repository": "acme/widgets", "pull_request": 7})
	if r.isError {
		t.Fatalf("answer = %+v", r)
	}
	var out reviewCommentsAnswer
	if err := json.Unmarshal([]byte(r.text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 105 || len(out.Comments) != 20 || out.Comments[0].Body != "comment 86" || out.Comments[19].Body != "comment 105" {
		t.Fatalf("out = %+v", out)
	}
	if out.Note != "Only the latest 20 of its 105 inline comments are here." {
		t.Errorf("note = %q", out.Note)
	}
	if got := commentPagesRead(f); !slices.Equal(got, []string{"6 of 20", "5 of 20"}) {
		t.Errorf("pages read %v", got)
	}
	for _, m := range mintedFor(f) {
		if m != "pull_requests=read widgets" {
			t.Errorf("mint = %q", m)
		}
	}
}

// commentPagesRead is each listing of inline comments the fake took:
// its page, and how many a page it asked for.
func commentPagesRead(f *fakeGitHub) []string {
	var out []string
	for _, q := range f.api() {
		if strings.HasSuffix(q.Path, "/comments") {
			out = append(out, q.Query.Get("page")+" of "+q.Query.Get("per_page"))
		}
	}
	return out
}

// Why this test exists: limit is the agent's to set, and an answer that
// ignored it would be short of what was asked for at 100 and long at 3.
// A page must also ask GitHub for no more than limit needs, since every
// comment on it is read whole, diff hunk and all, and one too large a
// page fails the call whatever the limit. At 100 against 250 comments
// both the answer and the pages are past the default's 20.
func TestListReviewCommentsHonoursTheLimit(t *testing.T) {
	for _, c := range []struct {
		limit       int
		first, last string
		pages       []string
	}{
		{3, "comment 248", "comment 250", []string{"84 of 3", "83 of 3"}},
		{1, "comment 250", "comment 250", []string{"250 of 1"}},
		{100, "comment 151", "comment 250", []string{"3 of 100", "2 of 100"}},
	} {
		g, f := newTest(t)
		f.route("GET /repos/acme/widgets/pulls/7", 200, `{"number":7,"review_comments":250}`)
		f.routeFunc("GET /repos/acme/widgets/pulls/7/comments", oldestFirst(250, reviewCommentItem))
		r := g.callT(t, agent(false, "acme/*"), "list_review_comments", map[string]any{"repository": "acme/widgets", "pull_request": 7, "limit": c.limit})
		var out reviewCommentsAnswer
		if err := json.Unmarshal([]byte(r.text), &out); err != nil {
			t.Fatalf("limit %d: %v", c.limit, err)
		}
		if n := len(out.Comments); n != c.limit || out.Comments[0].Body != c.first || out.Comments[n-1].Body != c.last {
			t.Errorf("limit %d: %d comments, from %q to %q", c.limit, n, out.Comments[0].Body, out.Comments[n-1].Body)
		}
		if want := fmt.Sprintf("Only the latest %d of its 250 inline comments are here.", c.limit); out.Note != want {
			t.Errorf("limit %d: note %q", c.limit, out.Note)
		}
		if got := commentPagesRead(f); !slices.Equal(got, c.pages) {
			t.Errorf("limit %d: pages read %v", c.limit, got)
		}
	}
}

// Why this test exists: reads of inline comments ask for a page of them
// at a time, so an answer past what Kivali reads fails however often it
// is asked. Words that say to try later send the agent round the same
// failure for good; it must be told to ask for fewer.
func TestListReviewCommentsTooLargeAsksForFewer(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/pulls/7", 200, `{"number":7,"review_comments":40}`)
	f.route("GET /repos/acme/widgets/pulls/7/comments", 200, `[{"body":"`+strings.Repeat("x", maxAnswerBytes)+`"}]`)
	r := g.callT(t, agent(false, "acme/*"), "list_review_comments", map[string]any{"repository": "acme/widgets", "pull_request": 7})
	want := "GitHub's answer with these comments is larger than Kivali reads, and trying again will not change that; ask for fewer with a smaller limit."
	if !r.isError || r.text != want {
		t.Errorf("answer = %+v", r)
	}
}

// Why this test exists: each field of an inline comment is what lets an
// agent find it in the code and answer it. An outdated comment has no
// line on the current diff, and reading it as line 0 would send the agent
// to the wrong place. A comment on a whole file has no line either, and
// subject_type is how the agent tells that from an outdated one. A reply
// must name what it answers; a long body is clipped with a note; and a
// diff hunk, which helps only while it is short, must not fill the
// answer. A pull request with no comments reads no listing, and a
// repository outside the access reaches nothing.
func TestListReviewCommentsCarriesWhatPlacesAComment(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/pulls/7", 200, `{"number":7,"review_comments":3}`)
	long := strings.Repeat("x", reviewBodyClip+10)
	listing, _ := json.Marshal([]map[string]any{
		reviewCommentItem(1),
		{"id": 2, "user": map[string]any{"login": "v"}, "path": "old.go", "line": nil, "original_line": 40, "side": "LEFT", "commit_id": headSHA, "in_reply_to_id": 1,
			"created_at": "2026-11-03T11:00:00Z", "html_url": "https://github.com/acme/widgets/pull/7#discussion_r2", "body": long, "diff_hunk": strings.Repeat("y", diffHunkMost+1)},
		{"id": 3, "user": nil, "path": "main.go", "subject_type": "file", "commit_id": headSHA, "body": "file-level"},
	})
	f.route("GET /repos/acme/widgets/pulls/7/comments", 200, string(listing))
	reader := agent(false, "acme/*")

	r := g.callT(t, reader, "list_review_comments", map[string]any{"repository": "acme/widgets", "pull_request": 7, "limit": 100})
	var out reviewCommentsAnswer
	if err := json.Unmarshal([]byte(r.text), &out); err != nil {
		t.Fatal(err, r.text)
	}
	if len(out.Comments) != 3 || out.Note != "A comment's body is cut to its first 5000 characters." {
		t.Fatalf("out = %+v", out)
	}
	first := out.Comments[0]
	if first.ID != 1 || first.Author != "u" || first.Path != "main.go" || first.Line == nil || *first.Line != 10 || first.OriginalLine == nil || *first.OriginalLine != 12 ||
		first.Side != "RIGHT" || first.Commit != headSHA || first.InReplyToID != 0 || first.CreatedAt != "2026-11-03T10:00:00Z" ||
		first.URL != "https://github.com/acme/widgets/pull/7#discussion_r1" || first.Body != "comment 1" || first.DiffHunk != "@@ -1 +1 @@\n-a\n+b" {
		t.Errorf("first = %+v", first)
	}
	second := out.Comments[1]
	if second.Line != nil || second.OriginalLine == nil || *second.OriginalLine != 40 || second.InReplyToID != 1 || len(second.Body) != reviewBodyClip || second.DiffHunk != "" {
		t.Errorf("second = %+v", second)
	}
	if third := out.Comments[2]; third.SubjectType != "file" || third.Line != nil {
		t.Errorf("third = %+v", third)
	}
	if strings.Contains(r.text, `"line":0`) || strings.Contains(r.text, `"line":null`) {
		t.Errorf("an absent line reads as one: %s", r.text)
	}

	f.route("GET /repos/acme/widgets/pulls/8", 200, `{"number":8,"review_comments":0}`)
	r = g.callT(t, reader, "list_review_comments", map[string]any{"repository": "acme/widgets", "pull_request": 8})
	if r.isError || r.text != `{"total":0,"comments":[]}` {
		t.Errorf("none = %+v", r)
	}
	if last := f.api()[len(f.api())-1]; strings.HasSuffix(last.Path, "/comments") {
		t.Errorf("asked %s for a pull request with none", last.Path)
	}

	before := len(f.requests())
	if r := g.callT(t, reader, "list_review_comments", map[string]any{"repository": "other/thing", "pull_request": 7}); r.refusal == nil {
		t.Errorf("another repository = %+v", r)
	}
	if len(f.requests()) != before {
		t.Error("a refused call reached GitHub")
	}
}

// Why this test exists: an answer is cut at 60,000 characters, from its
// end, and a list runs oldest first: long comments, as review bots write,
// would lose the newest ones, which are why the agent asked, and leave
// JSON that does not read. The oldest must go instead, and the note must
// count what is really there.
func TestLongBodiesCostTheOldestComments(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/pulls/7", 200, `{"number":7,"review_comments":100}`)
	f.routeFunc("GET /repos/acme/widgets/pulls/7/comments", oldestFirst(100, func(i int) map[string]any {
		c := reviewCommentItem(i)
		c["body"] = fmt.Sprintf("%d ", i) + strings.Repeat("z", reviewBodyClip)
		return c
	}))
	r := g.callT(t, agent(false, "acme/*"), "list_review_comments", map[string]any{"repository": "acme/widgets", "pull_request": 7, "limit": 100})
	var out reviewCommentsAnswer
	if err := json.Unmarshal([]byte(r.text), &out); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	n := len(out.Comments)
	if n == 0 || n >= 100 || !strings.HasPrefix(out.Comments[n-1].Body, "100 ") {
		t.Fatalf("%d comments, the last %.20q", n, out.Comments[n-1].Body)
	}
	if want := fmt.Sprintf("Only the latest %d of its 100 inline comments are here.", n); !strings.HasPrefix(out.Note, want) {
		t.Errorf("note = %q, want %q", out.Note, want)
	}
}

// Why this test exists: lastThatFit is what keeps a long list valid JSON,
// and its sums are the guard: what it counts must be the items and their
// commas, up to the room an answer leaves for the rest. Short by one
// either way, an answer near the bound keeps an item that pushes it past
// the cut, or loses one it had room for.
func TestFittingCountsEachItemAndItsComma(t *testing.T) {
	const short = 1000
	padding := func(over int) []string {
		// Each string takes its length, two quotes and a comma.
		long := answerClip - answerOtherRoom - 2*(short+3) - 3 + over
		return []string{strings.Repeat("a", long), strings.Repeat("b", short), strings.Repeat("c", short)}
	}
	if got := lastThatFit(padding(0)); len(got) != 3 {
		t.Errorf("exactly at the room: kept %d", len(got))
	}
	if got := lastThatFit(padding(1)); len(got) != 2 || got[0][0] != 'b' {
		t.Errorf("one over: kept %d", len(got))
	}
	first := func(over int) []string {
		s := padding(over)
		slices.Reverse(s)
		return s
	}
	room := answerClip - answerOtherRoom
	if got := firstThatFit(first(0), room); len(got) != 3 {
		t.Errorf("first, exactly at the room: kept %d", len(got))
	}
	if got := firstThatFit(first(1), room); len(got) != 2 || got[0][0] != 'c' {
		t.Errorf("first, one over: kept %d", len(got))
	}
}

// Why this test exists: an escape is what makes a body longer in the
// answer than it counts when clipped: a control character takes six. A
// fit measured on anything but the answer's JSON lets such bodies past
// the cut, which loses the newest and leaves text that does not parse.
func TestClipEncodedMeasuresTheEscapes(t *testing.T) {
	s := strings.Repeat("\x01", 100)
	for _, c := range []struct {
		most, keep int
		cut        bool
	}{{602, 100, false}, {601, 99, true}, {596, 99, true}, {595, 98, true}, {1, 0, true}} {
		got, cut := clipEncoded(s, c.most)
		if len(got) != c.keep || cut != c.cut {
			t.Errorf("most %d: kept %d, cut %v", c.most, len(got), cut)
		}
	}
	if got, _ := clipEncoded(strings.Repeat("é", 10), 7); got != "ééééé" {
		t.Errorf("cut inside a character: %q", got)
	}
}

// Why this test exists: reviews are written long by bots, and an answer
// cut from its end would lose the newest, the very ones the agent asked
// for, and stop being JSON. The oldest must go instead, however the
// length comes about (plain text, or escapes that count six times over
// in the answer), and the note must count what is really there, not what
// was read: an agent told it has the latest 20 and holding 11 would miss
// what lies between.
func TestLongReviewsCostTheOldest(t *testing.T) {
	for _, c := range []struct {
		name  string
		body  string
		limit int
	}{
		{"plain, to the limit", strings.Repeat("z", reviewBodyClip), 100},
		{"plain, past the limit", strings.Repeat("z", reviewBodyClip), 20},
		{"escapes", strings.Repeat("\x01", reviewBodyClip), 100},
	} {
		g, f := newTest(t)
		f.routeFunc("GET /repos/acme/widgets/pulls/7/reviews", oldestFirst(100, func(i int) map[string]any {
			r := reviewItem(i)
			r["body"] = c.body
			return r
		}))
		r := g.callT(t, agent(false, "acme/*"), "list_pull_request_reviews", map[string]any{"repository": "acme/widgets", "pull_request": 7, "limit": c.limit})
		var out reviewsAnswer
		if err := json.Unmarshal([]byte(r.text), &out); err != nil {
			t.Fatalf("%s: the answer is not JSON: %v", c.name, err)
		}
		n := len(out.Reviews)
		if n == 0 || n >= c.limit || out.Reviews[n-1].ID != 100 {
			t.Fatalf("%s: %d reviews", c.name, n)
		}
		if want := fmt.Sprintf("Only the latest %d of its 100 reviews are here.", n); !strings.HasPrefix(out.Note, want) {
			t.Errorf("%s: note %q, want %q", c.name, out.Note, want)
		}
	}
}

// Why this test exists: the room lastThatFit leaves is all that stands
// between a list that just fits and the answer's cut: without it, a list
// that fills answerClip alone leaves no room for the field names and the
// note around it, the note goes and the JSON with it. So the reviews step
// a character at a time across both edges: the one where the oldest must
// go, and the one the cut would fall on.
func TestAnAnswerAtTheMarginStaysJSON(t *testing.T) {
	const fixed = 14
	filler := strings.Repeat("b", 4000)
	item := func(i int, body string) reviewOut {
		return reviewOut{int64(i), "reviewer", "COMMENTED", headSHA, "2026-11-03T10:00:00Z", body}
	}
	taken := 0
	for i := 2; i <= fixed+1; i++ {
		taken += encodedLen(item(i, filler)) + 1
	}
	// bodyTo is how long the oldest review's body must be for the list to
	// take exactly edge characters.
	bodyTo := func(edge int) int { return edge - taken - encodedLen(item(1, "")) - 1 }

	for _, c := range []struct {
		name string
		edge int
		kept func(step int) int
	}{
		{"the room", answerClip - answerOtherRoom, func(step int) int {
			if step > 0 {
				return fixed
			}
			return fixed + 1
		}},
		{"the cut", answerClip, func(int) int { return fixed }},
	} {
		for step := -3; step <= 3; step++ {
			oldest := strings.Repeat("a", bodyTo(c.edge)+step)
			listing := []map[string]any{reviewItem(1)}
			listing[0]["body"] = oldest
			for i := 2; i <= fixed+1; i++ {
				r := reviewItem(i)
				r["body"] = filler
				listing = append(listing, r)
			}
			b, _ := json.Marshal(listing)
			g, f := newTest(t)
			f.route("GET /repos/acme/widgets/pulls/7/reviews", 200, string(b))
			r := g.callT(t, agent(false, "acme/*"), "list_pull_request_reviews", map[string]any{"repository": "acme/widgets", "pull_request": 7, "limit": 100})
			var out reviewsAnswer
			if err := json.Unmarshal([]byte(r.text), &out); err != nil {
				t.Fatalf("%s, step %d: the answer is not JSON: %v", c.name, step, err)
			}
			if len(out.Reviews) != c.kept(step) {
				t.Errorf("%s, step %d: kept %d", c.name, step, len(out.Reviews))
			}
			if n := len([]rune(r.text)); n > answerClip {
				t.Errorf("%s, step %d: %d characters", c.name, step, n)
			}
		}
	}
}
