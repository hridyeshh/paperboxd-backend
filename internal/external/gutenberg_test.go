package external

import "testing"

// A wrong match is worse than none: it puts a "Read free" button on a book
// that opens a different book.

func gb(id int, title, author string, copyright *bool) GutendexBook {
	b := GutendexBook{ID: id, Title: title, Copyright: copyright, Formats: map[string]string{
		"text/html":            "https://www.gutenberg.org/ebooks/1.html.images",
		"application/epub+zip": "https://www.gutenberg.org/ebooks/1.epub3.images",
	}}
	b.Authors = append(b.Authors, struct {
		Name string `json:"name"`
	}{author})
	return b
}

var (
	free   = new(bool)
	locked = func() *bool { b := true; return &b }()
)

func TestPickGutenbergMatchRequiresSameAuthor(t *testing.T) {
	results := []GutendexBook{gb(1, "The Road", "London, Jack", free)}
	if m := pickGutenbergMatch(results, normalizeTitle("The Road"), authorSurname([]string{"Cormac McCarthy"})); m != nil {
		t.Errorf("McCarthy's The Road matched Jack London's (id %d)", m.ID)
	}
}

func TestPickGutenbergMatchRequiresSameTitle(t *testing.T) {
	results := []GutendexBook{gb(1, "Dracula's Guest", "Stoker, Bram", free)}
	if m := pickGutenbergMatch(results, normalizeTitle("Dracula"), "stoker"); m != nil {
		t.Errorf("Dracula matched %q", m.Title)
	}
}

func TestPickGutenbergMatchSkipsCopyrightedAndUnknown(t *testing.T) {
	results := []GutendexBook{
		gb(1, "Dracula", "Stoker, Bram", locked),
		gb(2, "Dracula", "Stoker, Bram", nil),
		gb(3, "Dracula", "Stoker, Bram", free),
	}
	m := pickGutenbergMatch(results, "dracula", "stoker")
	if m == nil || m.ID != 3 {
		t.Fatalf("got %+v, want the copyright-free id 3", m)
	}
	if m.HTMLURL == "" || m.EPUBURL == "" {
		t.Errorf("formats not extracted: %+v", m)
	}
}

func TestPickGutenbergMatchNeedsHTML(t *testing.T) {
	b := gb(1, "Dracula", "Stoker, Bram", free)
	delete(b.Formats, "text/html")
	if m := pickGutenbergMatch([]GutendexBook{b}, "dracula", "stoker"); m != nil {
		t.Errorf("matched a book with no HTML text: %+v", m)
	}
}

func TestNormalizeTitleVariants(t *testing.T) {
	for _, title := range []string{
		"Frankenstein; Or, The Modern Prometheus",
		"Frankenstein, or The Modern Prometheus",
		"Frankenstein: The 1818 Text",
		"Frankenstein (Penguin Classics)",
	} {
		if got := normalizeTitle(title); got != "frankenstein" {
			t.Errorf("normalizeTitle(%q) = %q", title, got)
		}
	}
	if a, b := normalizeTitle("Pride & Prejudice"), normalizeTitle("Pride and Prejudice"); a != b {
		t.Errorf("%q != %q", a, b)
	}
}

func TestAuthorSurname(t *testing.T) {
	if got := authorSurname([]string{"Mary Wollstonecraft Shelley"}); got != "shelley" {
		t.Errorf("got %q", got)
	}
	if got := authorSurname(nil); got != "" {
		t.Errorf("no authors gave %q", got)
	}
}
