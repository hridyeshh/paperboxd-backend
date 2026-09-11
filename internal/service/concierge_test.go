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
