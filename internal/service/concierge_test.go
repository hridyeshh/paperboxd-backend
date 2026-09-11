package service

import (
	"strings"
	"testing"
)

// Jazy's one rule is: ask one question, and only when answering would be a
// coin flip. These pin both halves — that specific asks are answered, and
// that open ones get exactly one question with tappable options.

func TestClarifyingQuestionSilentForSpecificAsks(t *testing.T) {
	reader := ReaderContext{}
	for _, q := range []string{
		"books like Sally Rooney",
		"something under 300 pages",
		"something dark and fast-paced",
		"books about grief in postwar Japan",
		"something for my flight tomorrow",
		"sad but hopeful, no romance",
	} {
		if question, _ := clarifyingQuestion(ParseQuery(q), reader); question != "" {
			t.Errorf("%q: asked %q; the request was specific enough to answer", q, question)
		}
	}
}

func TestClarifyingQuestionAsksForOpenEndedAsks(t *testing.T) {
	reader := ReaderContext{}
	for _, q := range []string{
		"recommend me something",
		"what should I read next",
		"something good",
		"any ideas",
	} {
		question, opts := clarifyingQuestion(ParseQuery(q), reader)
		if question == "" {
			t.Errorf("%q: no question; this ask is a coin flip without one", q)
			continue
		}
		if len(opts) != 2 {
			t.Errorf("%q: %d options, want exactly 2 tappable replies", q, len(opts))
		}
	}
}

// For a reader we know, the useful question is comfort vs. stretch. For a
// stranger, pace.
func TestClarifyingQuestionDependsOnWhatWeKnow(t *testing.T) {
	stranger := ReaderContext{}
	known := ReaderContext{
		ReaderTaste: ReaderTaste{TotalRead: 40},
		TasteLines:  []string{"Quiet, character-driven stories"},
	}
	pq := ParseQuery("recommend me something")

	qs, _ := clarifyingQuestion(pq, stranger)
	qk, _ := clarifyingQuestion(pq, known)
	if qs == qk {
		t.Error("the same question was asked of a stranger and a known reader")
	}
	if !strings.Contains(strings.ToLower(qk), "usual") {
		t.Errorf("known reader got %q, want a comfort-vs-stretch question", qk)
	}
}

// The context block is what turns "ChatGPT that knows books" into a
// librarian. It must say the things that change a recommendation and stay
// silent when it knows nothing, rather than inventing.
func TestReaderContextPromptSection(t *testing.T) {
	empty := ReaderContext{}
	if got := empty.PromptSection(); !strings.Contains(got, "anonymous") {
		t.Errorf("empty context rendered %q; must tell the model not to invent history", got)
	}

	rc := ReaderContext{
		ReaderTaste:       ReaderTaste{TotalRead: 52, TopGenres: []string{"Literary Fiction"}, LovedBooks: []string{"Stoner"}},
		AvgPages:          310,
		VelocityBucket:    "slow",
		Abandoned:         []string{"The Way of Kings"},
		RejectedAs:        []string{"too much worldbuilding"},
		TBRCount:          23,
		SavedNeverStarted: 9,
		FriendsLoved:      []string{"The Bee Sting (by @maya)"},
		PreviousAsks:      []string{"something quiet"},
		TasteLines:        []string{"Quiet, character-driven stories"},
		RecentShift:       []string{"lately more drawn to bleak, unsparing books"},
	}
	got := rc.PromptSection()
	for _, want := range []string{
		"52 books", "310 pages", "slow reader", "Stoner",
		"The Way of Kings", "too much worldbuilding",
		"23 books saved", "saves a lot more than they start",
		"@maya", `"something quiet"`,
		"quiet, character-driven", "lately more drawn to",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("PromptSection missing %q:\n%s", want, got)
		}
	}
}

func TestReaderContextIsEmpty(t *testing.T) {
	if !(ReaderContext{}).IsEmpty() {
		t.Error("zero context reported non-empty")
	}
	if (ReaderContext{TBRCount: 1}).IsEmpty() {
		t.Error("a reader with a TBR reported empty")
	}
}

func TestLooksOpenEnded(t *testing.T) {
	for _, s := range []string{"recommend me something", "what should i read next", "any ideas?"} {
		if !looksOpenEnded(s) {
			t.Errorf("%q not recognised as open-ended", s)
		}
	}
	for _, s := range []string{"the brothers karamazov", "grief in postwar japan"} {
		if looksOpenEnded(s) {
			t.Errorf("%q wrongly flagged as open-ended", s)
		}
	}
}

// Phase 8: the two replies Jazy offers must change the ranking, or the
// question was a form field. "Surprise me" turns taste off and rewards leaving
// the reader's genres; "close to my usual" doubles taste.
func TestClarifyingAnswersSteerTaste(t *testing.T) {
	for q, want := range map[string]string{
		"recommend me something, Surprise me":      "surprise",
		"recommend me something, Close to my usual": "comfort",
		"something for my flight":                   "travel",
	} {
		if got := ParseQuery(q).Constraints.Context; got != want {
			t.Errorf("%q: context = %q, want %q", q, got, want)
		}
	}

	profile := &UserSignalProfile{GenreWeights: map[string]float64{"Fiction": 1}}
	inGenre := Candidate{VectorScore: 0.7, Categories: []string{"Fiction"}}
	outGenre := Candidate{VectorScore: 0.7, Categories: []string{"Science"}}

	surprise := SearchConstraints{Context: "surprise"}
	if searchScore(&inGenre, surprise, profile, false) >= searchScore(&outGenre, surprise, profile, false) {
		t.Error("surprise me: a book in the reader's usual genre outranked one outside it")
	}
	comfort := SearchConstraints{Context: "comfort"}
	if searchScore(&inGenre, comfort, profile, false) <= searchScore(&outGenre, comfort, profile, false) {
		t.Error("close to my usual: a book outside the reader's genre outranked one inside it")
	}
}

// Phase 7: the prompt must carry what the roadmap says Jazy knows — authors
// they return to and their own diary words — and cut diary text at a word.
func TestReaderContextCarriesAuthorsAndDiary(t *testing.T) {
	rc := ReaderContext{
		ReaderTaste: ReaderTaste{TotalRead: 12, LovedBooks: []string{"Stoner (5★)"}},
		TopAuthors:  []string{"Kazuo Ishiguro"},
		DiaryLines:  []string{diaryLine("Never Let Me Go", "This one wrecked me quietly over three evenings and I still think about the last page")},
	}
	got := rc.PromptSection()
	for _, want := range []string{"keeps coming back to: Kazuo Ishiguro", "Never Let Me Go: This one wrecked me", "Stoner (5★)"} {
		if !contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	long := diaryLine("T", "word "+strings.Repeat("x", 200))
	if len(long) > len("T: ")+ctxDiaryChars+len("…") || !strings.HasSuffix(long, "…") {
		t.Errorf("diary line not cut: %d chars %q", len(long), long)
	}
}
